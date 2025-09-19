package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/spf13/pflag"
)

const (
	maxParts          = 64               // максимальное количество частей
	minPartSize       = 2 * 1024 * 1024  // 2 MB минимальный размер части
	defaultBufSize    = 256 * 1024       // 256 KiB
	maxBufferSize     = 8 * 1024 * 1024  // 8 MiB
	defaultTimeout    = 60 * time.Second
	defaultRetries    = 5
	defaultRetryDelay = 2 * time.Second
	maxBackoff        = 30 * time.Second
)

type LogLevel int

const (
	LogError LogLevel = iota
	LogWarn
	LogInfo
	LogDebug
)

type Config struct {
	URL        string
	FilePath   string
	MaxWorkers int
	Timeout    time.Duration
	UserAgent  string
	Overwrite  bool
	LogLevel   LogLevel
	Verbose    bool
	BufferSize int
	NoEmoji    bool
	Retries    int
	RetryDelay time.Duration
	Resume     bool
	SaveState  bool
	Insecure   bool
}

func (c *Config) Validate() error {
	if c.URL == "" {
		return errors.New("URL не может быть пустым")
	}
	parsed, err := url.ParseRequestURI(c.URL)
	if err != nil {
		return fmt.Errorf("некорректный URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("поддерживаются только HTTP/HTTPS протоколы")
	}
	if c.MaxWorkers < 0 {
		return errors.New("количество воркеров не может быть отрицательным")
	}
	if c.Timeout <= 0 {
		return errors.New("таймаут должен быть положительным")
	}
	if c.BufferSize <= 0 {
		return errors.New("buffer-size должен быть положительным")
	}
	if c.BufferSize > maxBufferSize {
		return fmt.Errorf("buffer-size слишком большой (макс %d)", maxBufferSize)
	}
	if c.Retries < 0 {
		return errors.New("retries не может быть отрицательным")
	}
	if c.RetryDelay <= 0 {
		return errors.New("retry-delay должен быть положительным")
	}
	return nil
}

// ThreadSafeProgressBar — безопасная обёртка прогресс-бара
type ThreadSafeProgressBar struct {
	bar *progressbar.ProgressBar
	mu  sync.Mutex
}

func (tsb *ThreadSafeProgressBar) Add(num int) {
	if tsb.bar == nil {
		return
	}
	tsb.mu.Lock()
	_ = tsb.bar.Add(num)
	tsb.mu.Unlock()
}

// Add64 — метод для работы с int64, необходим для размеров файлов
func (tsb *ThreadSafeProgressBar) Add64(num int64) {
	if tsb.bar == nil {
		return
	}
	tsb.mu.Lock()
	_ = tsb.bar.Add64(num)
	tsb.mu.Unlock()
}

func (tsb *ThreadSafeProgressBar) Finish() error {
	if tsb.bar == nil {
		return nil
	}
	tsb.mu.Lock()
	defer tsb.mu.Unlock()
	return tsb.bar.Finish()
}

type PartInfo struct {
	Start      int64 `json:"start"`
	End        int64 `json:"end"`
	Downloaded int64 `json:"downloaded"`
	Completed  bool  `json:"completed"`
}

type Downloader struct {
	config          Config
	client          *http.Client
	clientNoH2      *http.Client
	ctx             context.Context
	cancel          context.CancelFunc
	filePath        string
	bar             *ThreadSafeProgressBar
	downloadedBytes int64
	bufPool         *sync.Pool
	rng             *rand.Rand
	partsMu         sync.Mutex // Для защиты parts и файла состояния
	parts           []PartInfo
}

func NewDownloader(cfg Config) (*Downloader, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Установка значений по умолчанию
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Go-MultiThread-Downloader/3.3.0"
	}
	if cfg.BufferSize == 0 {
		cfg.BufferSize = defaultBufSize
	}
	if cfg.Retries == 0 {
		cfg.Retries = defaultRetries
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = defaultRetryDelay
	}

	ctx, cancel := context.WithCancel(context.Background())

	defaultTransport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   maxParts,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	noH2Transport := defaultTransport.Clone()
	tlsConfig := &tls.Config{
		NextProtos: []string{"http/1.1"},
	}
	if cfg.Insecure {
		tlsConfig.InsecureSkipVerify = true
		fmt.Fprintf(os.Stderr, "⚠️ Отключена проверка TLS сертификатов (--insecure)\n")
	}
	noH2Transport.TLSClientConfig = tlsConfig

	if cfg.Insecure {
		defaultTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	pool := &sync.Pool{
		New: func() interface{} {
			buf := make([]byte, cfg.BufferSize)
			return &buf
		},
	}

	src := rand.NewSource(time.Now().UnixNano())
	rng := rand.New(src)

	return &Downloader{
		config:     cfg,
		client:     &http.Client{Transport: defaultTransport, Timeout: cfg.Timeout},
		clientNoH2: &http.Client{Transport: noH2Transport, Timeout: cfg.Timeout},
		ctx:        ctx,
		cancel:     cancel,
		bufPool:    pool,
		rng:        rng,
	}, nil
}

func (d *Downloader) Download() (err error) {
	d.setupSignalHandler()
	d.filePath = d.config.FilePath

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("паника: %v", r)
		}
		if d.bar != nil {
			_ = d.bar.Finish()
		}
		// Не удаляем файл, если включен save-state, чтобы можно было продолжить
		if err != nil && !errors.Is(err, context.Canceled) && !d.config.SaveState {
			d.cleanupPartialFile()
		}
	}()

	resp, err := d.getRemoteFileInfo()
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	contentLength, err := getContentLengthFromHeaders(resp.Header)
	if err != nil || contentLength <= 0 {
		return fmt.Errorf("не удалось определить размер файла: %w", err)
	}
	supportsRanges := strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")

	if err := d.prepareLocalFile(contentLength); err != nil {
		return err
	}

	numWorkers := d.calculateWorkerCount(contentLength, supportsRanges)
	d.logInfo("Информация: размер %s, потоки %d", formatBytes(contentLength), numWorkers)
	if !supportsRanges {
		d.logWarn("Сервер не поддерживает Range-запросы — будет однопоточная загрузка")
		numWorkers = 1
	}

	// Инициализируем или загружаем состояние частей
	d.initializeParts(contentLength, numWorkers)
	if d.config.Resume {
		if err := d.loadStateFile(); err != nil {
			d.logWarn("Не удалось загрузить состояние для resume: %v", err)
		}
	}

	initialDownloaded := d.calculateInitialDownloaded()
	if initialDownloaded == contentLength {
		d.logInfo("Файл уже скачан полностью (%s), пропускаю", formatBytes(contentLength))
		d.removeStateFile()
		return nil
	}

	out, err := os.OpenFile(d.config.FilePath, os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("не удалось открыть файл для записи: %w", err)
	}

	d.bar = &ThreadSafeProgressBar{bar: progressbar.DefaultBytes(contentLength, "скачивание")}
	if initialDownloaded > 0 {
		d.bar.Add64(initialDownloaded)
		atomic.StoreInt64(&d.downloadedBytes, initialDownloaded)
	}

	var downloadErr error
	if numWorkers == 1 {
		downloadErr = d.downloadSingle(out, contentLength, initialDownloaded, supportsRanges)
	} else {
		downloadErr = d.downloadMulti(out)
	}

	if downloadErr != nil {
		out.Close()
		return downloadErr
	}

	if closeErr := out.Close(); closeErr != nil {
		return fmt.Errorf("ошибка закрытия файла: %w", closeErr)
	}

	// Проверяем размер файла ПОСЛЕ закрытия
	finalStat, statErr := os.Stat(d.config.FilePath)
	if statErr != nil {
		return fmt.Errorf("ошибка проверки итогового размера: %w", statErr)
	}

	if finalStat.Size() != contentLength {
		return fmt.Errorf("файл повреждён: ожидаемый %d, получен %d", contentLength, finalStat.Size())
	}

	d.logInfo("✅ Успешно: %s (%s)", d.config.FilePath, formatBytes(contentLength))
	d.removeStateFile()
	return nil
}

func (d *Downloader) prepareLocalFile(contentLength int64) error {
	if err := d.checkFileExists(); err != nil {
		return err
	}
	if err := d.checkDiskSpace(contentLength); err != nil {
		return err
	}

	// Если файл существует и мы не в режиме resume, он будет перезаписан.
	// Если в режиме resume, мы будем писать поверх.
	// Truncate нужен только для нового файла.
	stat, err := os.Stat(d.config.FilePath)
	if os.IsNotExist(err) || (!d.config.Resume && stat.Size() != contentLength) {
		d.logInfo("Создание файла и выделение места на диске...")
		out, createErr := os.Create(d.config.FilePath)
		if createErr != nil {
			return fmt.Errorf("ошибка создания файла: %w", createErr)
		}
		// Truncate может быть медленным, но он предотвращает ошибки "нет места на диске" в середине загрузки
		if truncErr := out.Truncate(contentLength); truncErr != nil {
			out.Close()
			return fmt.Errorf("ошибка выделения места: %w", truncErr)
		}
		out.Close()
	} else if err == nil && d.config.Resume {
		d.logInfo("Продолжение загрузки файла %s", d.config.FilePath)
	}

	return nil
}

func (d *Downloader) checkDiskSpace(required int64) error {
	dir := filepath.Dir(d.config.FilePath)
	if err := d.checkDiskSpacePlatform(dir, required); err == nil {
		return nil
	} else {
		d.logDebug("Прямая проверка диска недоступна (%v), использую fallback", err)
	}

	temp, err := os.CreateTemp(dir, "downloader_space_*")
	if err != nil {
		return fmt.Errorf("не удалось проверить пространство (создание temp): %w", err)
	}
	defer os.Remove(temp.Name())
	defer temp.Close()

	if err := temp.Truncate(required); err != nil {
		return fmt.Errorf("недостаточно дискового пространства для файла %s", formatBytes(required))
	}
	return nil
}

func (d *Downloader) getRemoteFileInfo() (*http.Response, error) {
	d.logInfo("Получаю информацию о файле...")
	req, err := http.NewRequestWithContext(d.ctx, "HEAD", d.config.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания HEAD: %w", err)
	}
	req.Header.Set("User-Agent", d.config.UserAgent)

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка HEAD: %w", err)
	}

	if resp.StatusCode == http.StatusOK && resp.Header.Get("Content-Length") != "" {
		return resp, nil
	}
	resp.Body.Close()
	d.logWarn("HEAD вернул %s или не содержит Content-Length, пробую GET Range 0-0", resp.Status)
	return d.getRemoteFileInfoFallback()
}

func (d *Downloader) getRemoteFileInfoFallback() (*http.Response, error) {
	req, err := http.NewRequestWithContext(d.ctx, "GET", d.config.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания GET fallback: %w", err)
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", d.config.UserAgent)

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка GET fallback: %w", err)
	}
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("fallback вернул статус %s", resp.Status)
	}
	return resp, nil
}

func getContentLengthFromHeaders(h http.Header) (int64, error) {
	if cl := h.Get("Content-Length"); cl != "" {
		if v, err := strconv.ParseInt(cl, 10, 64); err == nil && v > 0 {
			return v, nil
		}
	}
	if cr := h.Get("Content-Range"); cr != "" {
		parts := strings.Split(cr, "/")
		if len(parts) == 2 {
			total := strings.TrimSpace(parts[1])
			if total != "" && total != "*" {
				if v, err := strconv.ParseInt(total, 10, 64); err == nil && v > 0 {
					return v, nil
				}
			}
		}
	}
	return 0, errors.New("Content-Length и Content-Range отсутствуют или некорректны")
}

func (d *Downloader) downloadSingle(writer io.WriterAt, contentLength int64, existingSize int64, supportsRanges bool) error {
	req, err := http.NewRequestWithContext(d.ctx, "GET", d.config.URL, nil)
	if err != nil {
		return fmt.Errorf("создание GET: %w", err)
	}
	if d.config.Resume && existingSize > 0 && supportsRanges {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existingSize))
	}
	req.Header.Set("User-Agent", d.config.UserAgent)

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		d.logInfo("Файл уже полностью скачан (сервер вернул 416).")
		return nil
	}

	expectedStatus := http.StatusOK
	if d.config.Resume && existingSize > 0 && supportsRanges {
		expectedStatus = http.StatusPartialContent
	}
	if resp.StatusCode != expectedStatus {
		return fmt.Errorf("сервер вернул %s, ожидался %d", resp.Status, expectedStatus)
	}

	if resp.StatusCode == http.StatusPartialContent {
		cr := resp.Header.Get("Content-Range")
		if cr != "" {
			// формат: bytes START-END/TOTAL
			if parts := strings.Split(cr, "/"); len(parts) == 2 {
				totalStr := strings.TrimSpace(parts[1])
				if total, err := strconv.ParseInt(totalStr, 10, 64); err == nil {
					if total != contentLength {
						return fmt.Errorf("файл на сервере изменился: ожидался размер %d, получен %d", contentLength, total)
					}
				}
			}
		}
	}


	reader := &progressReader{reader: resp.Body, bar: d.bar}
	bufPtr := d.bufPool.Get().(*[]byte)
	defer d.bufPool.Put(bufPtr)

	ow := &offsetWriter{writer: writer, offset: existingSize}
	_, err = io.CopyBuffer(ow, reader, *bufPtr)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		d.logWarn("Single mode: соединение прервано (частичная загрузка)")
	}
	return nil
}

type progressReader struct {
	reader io.Reader
	bar    *ThreadSafeProgressBar
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.reader.Read(p)
	if n > 0 {
		pr.bar.Add(n)
	}
	return
}

type offsetWriter struct {
	writer io.WriterAt
	offset int64
}

func (ow *offsetWriter) Write(p []byte) (n int, err error) {
	n, err = ow.writer.WriteAt(p, ow.offset)
	if err == nil {
		ow.offset += int64(n)
	}
	return
}

func (d *Downloader) downloadMulti(out *os.File) error {
	incompleteParts := make([]int, 0, len(d.parts))
	for i, part := range d.parts {
		if !part.Completed {
			incompleteParts = append(incompleteParts, i)
		}
	}

	if len(incompleteParts) == 0 {
		d.logInfo("Все части уже скачаны (согласно файлу состояния)")
		return nil
	}

	d.logInfo("Скачиваю %d незавершённых частей из %d", len(incompleteParts), len(d.parts))

	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()

	errChan := make(chan error, len(incompleteParts))
	var wg sync.WaitGroup

	for _, partIdx := range incompleteParts {
		wg.Add(1)
		go d.downloadPart(ctx, &wg, errChan, out, partIdx)
	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	for e := range errChan {
		if e != nil {
			cancel()
			return fmt.Errorf("ошибка при скачивании части: %w", e)
		}
	}
	return nil
}

func (d *Downloader) loadStateFile() error {
	if !d.config.SaveState {
		return nil
	}
	stateFile := d.filePath + ".parts"
	data, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		return nil // Файла нет, это нормально
	}
	if err != nil {
		return fmt.Errorf("чтение файла состояния: %w", err)
	}

	d.partsMu.Lock()
	defer d.partsMu.Unlock()
	return json.Unmarshal(data, &d.parts)
}

func (d *Downloader) saveStateFileUnsafe() error {
	if !d.config.SaveState || len(d.parts) == 0 {
		return nil
	}
	stateFile := d.filePath + ".parts"
	data, err := json.MarshalIndent(d.parts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, data, 0644)
}

func (d *Downloader) removeStateFile() {
	if !d.config.SaveState {
		return
	}
	stateFile := d.filePath + ".parts"
	if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
		d.logWarn("Не удалось удалить файл состояния %s: %v", stateFile, err)
	} else {
		d.logDebug("Файл состояния удалён: %s", stateFile)
	}
}

func (d *Downloader) initializeParts(contentLength int64, numWorkers int) {
	d.partsMu.Lock()
	defer d.partsMu.Unlock()

	// Не пересоздаем части, если они уже загружены из файла состояния и их число совпадает
	if len(d.parts) == numWorkers {
		return
	}

	d.parts = make([]PartInfo, numWorkers)
	partSize := contentLength / int64(numWorkers)

	for i := 0; i < numWorkers; i++ {
		start := int64(i) * partSize
		end := start + partSize - 1
		if i == numWorkers-1 {
			end = contentLength - 1
		}
		d.parts[i] = PartInfo{Start: start, End: end}
	}
}

func (d *Downloader) calculateInitialDownloaded() int64 {
	d.partsMu.Lock()
	defer d.partsMu.Unlock()
	var total int64
	for _, p := range d.parts {
		total += p.Downloaded
	}
	return total
}

func (d *Downloader) downloadPart(ctx context.Context, wg *sync.WaitGroup, errChan chan<- error, out *os.File, partIndex int) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			errChan <- fmt.Errorf("паника в части %d: %v", partIndex+1, r)
		}
	}()

	for attempt := 0; attempt <= d.config.Retries; attempt++ {
		if ctx.Err() != nil {
			return
		}

		d.partsMu.Lock()
		part := d.parts[partIndex]
		d.partsMu.Unlock()

		start := part.Start + part.Downloaded
		end := part.End

		if start > end { // Часть уже скачана
			return
		}

		if attempt > 0 {
			backoff := d.config.RetryDelay * (1 << (attempt - 1))
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			jitter := 0.5 + d.rng.Float64()
			backoff = time.Duration(float64(backoff) * jitter)
			d.logWarn("Часть %d: повтор %d через %v", partIndex+1, attempt, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}

		written, err := d.performPartDownload(ctx, out, partIndex+1, start, end)

		if written > 0 {
			d.partsMu.Lock()
			d.parts[partIndex].Downloaded += written
			// Сохраняем прогресс даже при ошибке, если что-то скачалось
			if d.config.SaveState {
				if saveErr := d.saveStateFileUnsafe(); saveErr != nil {
					d.logWarn("Не удалось сохранить промежуточное состояние: %v", saveErr)
				}
			}
			d.partsMu.Unlock()
		}

		if err == nil {
			d.markPartAsCompleteAndSave(partIndex)
			return // Успех
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}

		if errors.Is(err, io.ErrUnexpectedEOF) {
			d.logWarn("Часть %d: соединение прервано (частичная загрузка), будет повтор", partIndex+1)
		} else {
			d.logError("Часть %d: ошибка: %v", partIndex+1, err)
		}

		if attempt == d.config.Retries {
			errChan <- fmt.Errorf("часть %d: исчерпаны попытки: %w", partIndex+1, err)
			return
		}
	}
}

func (d *Downloader) markPartAsCompleteAndSave(partIndex int) {
	d.partsMu.Lock()
	defer d.partsMu.Unlock()

	part := &d.parts[partIndex]
	part.Completed = true
	part.Downloaded = part.End - part.Start + 1

	if d.config.SaveState {
		if err := d.saveStateFileUnsafe(); err != nil {
			d.logWarn("Не удалось сохранить финальное состояние части %d: %v", partIndex+1, err)
		}
	}
	d.logDebug("Часть %d завершена и сохранена", partIndex+1)
}

func (d *Downloader) performPartDownload(ctx context.Context, out *os.File, partID int, start, end int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", d.config.URL, nil)
	if err != nil {
		return 0, fmt.Errorf("создание запроса: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("User-Agent", d.config.UserAgent)

	resp, err := d.clientNoH2.Do(req)
	if err != nil {
		return 0, fmt.Errorf("http запрос: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return 0, fmt.Errorf("сервер отверг диапазон %d-%d", start, end)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("неожиданный статус %s (ожидался 206)", resp.Status)
	}

	if err := d.validateContentRange(resp.Header, start, end); err != nil {
		return 0, fmt.Errorf("валидация Content-Range: %w", err)
	}

	expectedSize := end - start + 1
	reader := &progressReader{reader: resp.Body, bar: d.bar}
	ow := &offsetWriter{writer: out, offset: start}
	bufPtr := d.bufPool.Get().(*[]byte)
	defer d.bufPool.Put(bufPtr)

	written, copyErr := io.CopyBuffer(ow, reader, *bufPtr)

	if copyErr != nil {
		if errors.Is(copyErr, io.ErrUnexpectedEOF) {
			d.logDebug("Часть %d: короткое чтение (%d из %d байт)", partID, written, expectedSize)
			return written, copyErr // Возвращаем для retry
		}
		return written, copyErr
	}

	if written != expectedSize {
		return written, fmt.Errorf("часть %d: записано %d байт, ожидалось %d", partID, written, expectedSize)
	}
	return written, nil
}

func (d *Downloader) validateContentRange(headers http.Header, expectedStart, expectedEnd int64) error {
	cr := headers.Get("Content-Range")
	if cr == "" {
		return errors.New("отсутствует Content-Range заголовок")
	}

	// Пример: bytes 200-1000/67589
	_, after, found := strings.Cut(cr, " ")
	if !found {
		return fmt.Errorf("некорректный формат Content-Range: %s", cr)
	}
	rangeAndTotal, _, found := strings.Cut(after, "/")
	if !found {
		return fmt.Errorf("некорректный формат Content-Range: %s", cr)
	}
	startStr, endStr, found := strings.Cut(rangeAndTotal, "-")
	if !found {
		return fmt.Errorf("некорректный формат диапазона в Content-Range: %s", rangeAndTotal)
	}

	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return fmt.Errorf("некорректный start в Content-Range: %s", startStr)
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil {
		return fmt.Errorf("некорректный end в Content-Range: %s", endStr)
	}

	if start != expectedStart || end != expectedEnd {
		return fmt.Errorf("диапазон не соответствует запрошенному: получен %d-%d, ожидался %d-%d", start, end, expectedStart, expectedEnd)
	}
	return nil
}

func (d *Downloader) calculateWorkerCount(contentLength int64, supportsRanges bool) int {
	if !supportsRanges || contentLength < minPartSize {
		return 1
	}
	if d.config.MaxWorkers > 0 {
		return minInt(d.config.MaxWorkers, maxParts)
	}

	numCPU := runtime.NumCPU()
	if numCPU < 1 {
		numCPU = 1
	}
	// Эвристика: не более 4 потоков на ядро, но не более maxParts
	guess := minInt(numCPU*4, maxParts)

	// Убедимся, что размер части не слишком маленький
	if contentLength/int64(guess) < minPartSize {
		guess = int(contentLength / minPartSize)
	}

	if guess < 1 {
		return 1
	}
	return minInt(guess, maxParts)
}

// Логирование
func (d *Downloader) logDebug(format string, args ...interface{}) {
	if d.config.LogLevel >= LogDebug {
		prefix := ""
		if !d.config.NoEmoji {
			prefix = "🔍 "
		}
		fmt.Fprintf(os.Stderr, prefix+format+"\n", args...)
	}
}

func (d *Downloader) logInfo(format string, args ...interface{}) {
	if d.config.LogLevel >= LogInfo {
		prefix := ""
		if !d.config.NoEmoji {
			prefix = "ℹ️  "
		}
		fmt.Fprintf(os.Stderr, prefix+format+"\n", args...)
	}
}

func (d *Downloader) logWarn(format string, args ...interface{}) {
	if d.config.LogLevel >= LogWarn {
		prefix := ""
		if !d.config.NoEmoji {
			prefix = "⚠️  "
		}
		fmt.Fprintf(os.Stderr, prefix+format+"\n", args...)
	}
}

func (d *Downloader) logError(format string, args ...interface{}) {
	if d.config.LogLevel >= LogError {
		prefix := ""
		if !d.config.NoEmoji {
			prefix = "❌ "
		}
		fmt.Fprintf(os.Stderr, prefix+format+"\n", args...)
	}
}

// cleanupPartialFile удаляет файл при ошибке
func (d *Downloader) cleanupPartialFile() {
	if d.filePath == "" {
		return
	}
	if err := os.Remove(d.filePath); err != nil && !os.IsNotExist(err) {
		d.logError("Не удалось удалить частичный файл: %v", err)
	} else if err == nil {
		d.logInfo("Частичный файл удалён: %s", d.filePath)
	}
}

// setupSignalHandler ловит Ctrl-C для сохранения состояния и плавной остановки
func (d *Downloader) setupSignalHandler() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		d.logWarn("\nСигнал прерывания получен, сохраняю состояние...")

		if d.config.SaveState {
			// Блокируем мьютекс, чтобы безопасно прочитать состояние d.parts
			d.partsMu.Lock()
			if err := d.saveStateFileUnsafe(); err != nil {
				d.logError("Не удалось сохранить состояние при выходе: %v", err)
			} else {
				d.logInfo("Состояние успешно сохранено в %s.parts", d.filePath)
			}
			d.partsMu.Unlock()
		}

		// Отменяем контекст, чтобы все воркеры завершились
		d.cancel()
	}()
}

// checkFileExists спрашивает перезапись если нужно
func (d *Downloader) checkFileExists() error {
	_, err := os.Stat(d.config.FilePath)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("ошибка проверки файла: %w", err)
	}

	if d.config.Overwrite || d.config.Resume {
		var mode string
		if d.config.Resume {
			mode = "продолжаю"
		} else {
			mode = "перезаписываю"
		}
		d.logInfo("Файл %s существует — %s", d.config.FilePath, mode)
		return nil
	}

	d.logWarn("Файл %s уже существует.", d.config.FilePath)
	fmt.Print("Перезаписать? (y/N): ")
	var response string
	_, err = fmt.Scanln(&response)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("ошибка чтения ответа: %w", err)
	}
	response = strings.ToLower(strings.TrimSpace(response))
	if response != "y" && response != "yes" {
		return errors.New("операция отменена пользователем")
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func basenameFromURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	base := filepath.Base(parsed.Path)
	if base == "." || base == "/" || base == "" {
		return "", errors.New("не удалось определить имя файла из URL")
	}
	decoded, err := url.QueryUnescape(base)
	if err != nil {
		return base, nil // Возвращаем нераскодированное имя, если раскодировать не удалось
	}
	return decoded, nil
}

// --- CLI парсинг ---
func parseFlags() (Config, error) {
	var cfg Config
	var logLevelStr string
	pflag.StringVarP(&cfg.URL, "url", "u", "", "URL файла (обязательно)")
	pflag.StringVarP(&cfg.FilePath, "output", "o", "", "Путь к выходному файлу (по умолчанию — имя из URL)")
	pflag.IntVarP(&cfg.MaxWorkers, "workers", "w", 0, "Количество потоков (0 = авто)")
	pflag.DurationVarP(&cfg.Timeout, "timeout", "t", defaultTimeout, "Таймаут HTTP-запроса")
	pflag.StringVarP(&cfg.UserAgent, "user-agent", "a", "", "User-Agent")
	pflag.BoolVarP(&cfg.Overwrite, "force", "f", false, "Перезаписать без подтверждения")
	pflag.BoolVarP(&cfg.Verbose, "verbose", "v", false, "Подробный вывод (эквивалент --log-level=debug)")
	pflag.StringVar(&logLevelStr, "log-level", "info", "Уровень логов: error,warn,info,debug")
	pflag.IntVar(&cfg.BufferSize, "buffer-size", defaultBufSize, "Размер буфера (байт, max 8MiB)")
	pflag.BoolVar(&cfg.NoEmoji, "no-emoji", false, "Отключить эмодзи в логах")
	pflag.IntVar(&cfg.Retries, "retries", defaultRetries, "Количество повторов на часть")
	pflag.DurationVar(&cfg.RetryDelay, "retry-delay", defaultRetryDelay, "Базовая задержка повтора")
	pflag.BoolVar(&cfg.Resume, "resume", false, "Продолжить скачивание существующего файла (если возможно)")
	pflag.BoolVar(&cfg.SaveState, "save-state", true, "Сохранять состояние частей для надёжного resume (включено по умолчанию)")
	pflag.BoolVarP(&cfg.Insecure, "insecure", "k", false, "Игнорировать ошибки TLS сертификатов")

	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Использование: %s [флаги] <URL>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Примеры:\n")
		fmt.Fprintf(os.Stderr, "  %s https://example.com/largefile.zip\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -w 8 -o my_file.bin --resume -v https://speed.hetzner.de/100MB.bin\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s --insecure https://self-signed.badssl.com/file.zip\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Флаги:\n")
		pflag.PrintDefaults()
	}
	pflag.Parse()

	// Если указан флаг resume, автоматически включаем save-state
	if cfg.Resume {
		cfg.SaveState = true
	}

	if cfg.URL == "" {
		if pflag.NArg() > 0 {
			cfg.URL = pflag.Arg(0)
		} else {
			pflag.Usage()
			return cfg, errors.New("флаг --url или позиционный аргумент с URL обязателен")
		}
	}

	if cfg.Verbose {
		cfg.LogLevel = LogDebug
	} else {
		switch strings.ToLower(logLevelStr) {
		case "debug":
			cfg.LogLevel = LogDebug
		case "info":
			cfg.LogLevel = LogInfo
		case "warn":
			cfg.LogLevel = LogWarn
		case "error":
			cfg.LogLevel = LogError
		default:
			return cfg, fmt.Errorf("некорректный --log-level: %s", logLevelStr)
		}
	}

	if cfg.FilePath == "" {
		if name, err := basenameFromURL(cfg.URL); err == nil {
			cfg.FilePath = name
		} else {
			// Создаем временный логгер, т.к. основной еще не инициализирован
			tempCfg := cfg
			tempCfg.LogLevel = LogWarn
			d, _ := NewDownloader(tempCfg)
			d.logWarn("Не удалось определить имя файла из URL, будет использовано 'downloaded_file'")
			cfg.FilePath = "downloaded_file"
		}
	}
	return cfg, nil
}

func main() {
	cfg, err := parseFlags()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка парсинга флагов: %v\n", err)
		os.Exit(1)
	}

	dl, err := NewDownloader(cfg)
	if err != nil {
		// Используем fmt.Fprintf, т.к. логгер dl может быть nil
		fmt.Fprintf(os.Stderr, "Ошибка создания downloader: %v\n", err)
		os.Exit(1)
	}

	if err := dl.Download(); err != nil {
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "операция отменена пользователем") {
			dl.logError("Ошибка скачивания: %v", err)
		} else {
			dl.logInfo("Загрузка отменена.")
		}
		os.Exit(1)
	}
}
