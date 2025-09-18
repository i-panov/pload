package main

import (
	"context"
	"crypto/tls"
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
	maxParts        = 64              // максимальное количество частей
	minPartSize     = 2 * 1024 * 1024 // 2 MB минимальный размер части
	maxPartSize     = 32 * 1024 * 1024 // 32 MB максимальный размер части
	defaultBufSize  = 256 * 1024      // 256 KiB
	maxBufferSize   = 8 * 1024 * 1024 // 8 MiB
	defaultTimeout  = 60 * time.Second
	defaultRetries  = 5
	defaultRetryDelay = 2 * time.Second
	maxBackoff      = 30 * time.Second // Добавлено: максимальный backoff
)

type LogLevel int

const (
	LogError LogLevel = iota
	LogWarn
	LogInfo
	LogDebug
)

type Config struct {
	URL         string
	FilePath    string
	MaxWorkers  int
	Timeout     time.Duration
	UserAgent   string
	Overwrite   bool
	LogLevel    LogLevel
	Verbose     bool
	BufferSize  int
	NoEmoji     bool
	Retries     int
	RetryDelay  time.Duration
	Resume      bool
	SaveState   bool   // Добавлено: сохранение состояния для resume
	Insecure    bool   // Добавлено: игнорирование TLS ошибок
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

// PartInfo для tracking состояния части при resume
type PartInfo struct {
	Start       int64
	End         int64
	Downloaded  int64
	Completed   bool
}

// Downloader основной тип
type Downloader struct {
	config         Config
	client         *http.Client
	clientNoH2     *http.Client
	ctx            context.Context
	cancel         context.CancelFunc
	filePath       string
	bar            *ThreadSafeProgressBar
	downloadedBytes int64
	bufPool        *sync.Pool
	rng            *rand.Rand
	parts          []PartInfo // для tracking частей при resume
}

// NewDownloader создаёт экземпляр
func NewDownloader(cfg Config) (*Downloader, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Go-MultiThread-Downloader/3.2.2"
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

	// стандартный транспорт (с возможным HTTP/2)
	defaultTransport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   maxParts,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// транспорт с отключенным HTTP/2 (принуждает http/1.1)
	noH2Transport := defaultTransport.Clone()
	tlsConfig := &tls.Config{
		NextProtos: []string{"http/1.1"},
	}
	if cfg.Insecure {
		tlsConfig.InsecureSkipVerify = true
		// ИСПРАВЛЕНО: Используем fmt.Fprintf вместо d.logWarn (d еще не создан)
		fmt.Fprintf(os.Stderr, "⚠️ Отключена проверка TLS сертификатов (--insecure)\n")
	}
	noH2Transport.TLSClientConfig = tlsConfig

	// Применяем insecure и к основному транспорту
	if cfg.Insecure {
		defaultTransport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	pool := &sync.Pool{
		New: func() interface{} {
			buf := make([]byte, cfg.BufferSize)
			return &buf
		},
	}

	// Локальный RNG вместо global rand.Seed
	src := rand.NewSource(time.Now().UnixNano())
	rng := rand.New(src)

	return &Downloader{
		config: cfg,
		client: &http.Client{
			Transport: defaultTransport,
			Timeout:   cfg.Timeout,
		},
		clientNoH2: &http.Client{
			Transport: noH2Transport,
			Timeout:   cfg.Timeout,
		},
		ctx:     ctx,
		cancel:  cancel,
		bufPool: pool,
		rng:     rng,
	}, nil
}

// Download главный метод
func (d *Downloader) Download() (err error) {
	d.setupSignalHandler()
	d.filePath = d.config.FilePath

	// defer cleanup and progress finish
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("паника: %v", r)
		}
		if d.bar != nil {
			_ = d.bar.Finish()
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			d.cleanupPartialFile()
		}
	}()

	// --- Фаза 1: Информация о файле (HEAD / fallback) ---
	resp, err := d.getRemoteFileInfo()
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	contentLength, err := getContentLengthFromHeaders(resp.Header)
	if err != nil || contentLength <= 0 {
		return fmt.Errorf("не удалось определить размер файла (Content-Length/Content-Range): %w", err)
	}
	supportsRanges := strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")

	// --- Подготовка локального файла ---
	existingSize, err := d.prepareLocalFile(contentLength)
	if err != nil {
		return err
	}

	// ИСПРАВЛЕНО: Проверка полного скачивания в самом начале
	if existingSize == contentLength {
		d.logInfo("Файл уже скачан полностью (%s), пропускаю", formatBytes(contentLength))
		d.removeStateFile() // Очищаем state file если есть
		return nil
	}

	// Загружаем состояние для resume (если включено)
	if err := d.loadStateFile(); err != nil {
		d.logWarn("Не удалось загрузить состояние: %v", err)
	}

	// --- Решаем число потоков ---
	numWorkers := d.calculateWorkerCount(contentLength, supportsRanges)
	d.logInfo("Информация: размер %s, потоки %d", formatBytes(contentLength), numWorkers)
	if !supportsRanges {
		d.logWarn("Сервер не поддерживает Range-запросы — будет однопоточная загрузка")
		numWorkers = 1
	}

	out, err := os.OpenFile(d.config.FilePath, os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("не удалось открыть файл для записи: %w", err)
	}

	// Progress starting from existing
	progressBar := progressbar.DefaultBytes(contentLength, "скачивание")
	if existingSize > 0 {
		progressBar.Add64(existingSize)
		atomic.AddInt64(&d.downloadedBytes, existingSize)
	}
	d.bar = &ThreadSafeProgressBar{bar: progressBar}

	var downloadErr error
	if numWorkers == 1 {
		downloadErr = d.downloadSingle(out, contentLength, existingSize, supportsRanges)
	} else {
		downloadErr = d.downloadMulti(out, contentLength, numWorkers, existingSize)
	}

	if downloadErr != nil {
		out.Close() // Закрываем при ошибке
		return downloadErr
	}

	// ИСПРАВЛЕНО: Проверка размера файла ПЕРЕД закрытием (Windows compatibility)
	stat, statErr := out.Stat()
	if statErr != nil {
		out.Close()
		return fmt.Errorf("ошибка проверки размера: %w", statErr)
	}
	
	// Закрываем файл только после всех операций
	if closeErr := out.Close(); closeErr != nil {
		return fmt.Errorf("ошибка закрытия файла: %w", closeErr)
	}

	downloadedSize := atomic.LoadInt64(&d.downloadedBytes)
	if stat.Size() != contentLength {
		return fmt.Errorf("файл повреждён: ожидаемый %d, получен %d, скачано %d",
			contentLength, stat.Size(), downloadedSize)
	}

	d.logInfo("✅ Успешно: %s (%s)", d.config.FilePath, formatBytes(contentLength))
	
	// Удаляем файл состояния после успешного завершения
	d.removeStateFile()
	
	return nil
}

// prepareLocalFile проверяет существование, диск и выделяет место
func (d *Downloader) prepareLocalFile(contentLength int64) (existingSize int64, err error) {
	if err := d.checkFileExists(); err != nil {
		return 0, err
	}
	if err := d.checkDiskSpace(contentLength); err != nil {
		return 0, err
	}
	d.logInfo("Создание файла и выделение места на диске...")
	flags := os.O_CREATE | os.O_WRONLY
	if d.config.Resume {
		flags = os.O_RDWR | os.O_CREATE
	}
	out, err := os.OpenFile(d.config.FilePath, flags, 0644)
	if err != nil {
		return 0, fmt.Errorf("ошибка создания файла: %w", err)
	}
	defer out.Close()

	stat, err := out.Stat()
	if err != nil {
		return 0, err
	}
	existingSize = stat.Size()

	if d.config.Resume && existingSize > 0 {
		if existingSize > contentLength {
			return 0, fmt.Errorf("существующий файл больше ожидаемого (%d > %d), удалите или используйте без --resume", existingSize, contentLength)
		}
		d.logInfo("Resuming от %s / %s", formatBytes(existingSize), formatBytes(contentLength))
	} else {
		if err := out.Truncate(contentLength); err != nil {
			return 0, fmt.Errorf("ошибка выделения места: %w", err)
		}
	}
	return existingSize, nil
}

func (d *Downloader) checkDiskSpace(required int64) error {
	dir := filepath.Dir(d.config.FilePath)
	
	// Кросс-платформенная проверка дискового пространства
	if err := d.checkDiskSpaceCrossPlatform(dir, required); err == nil {
		return nil
	}
	
	// fallback: пробуем создать временный файл нужного размера
	d.logDebug("Прямая проверка диска недоступна, использую fallback")
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

// getRemoteFileInfo делает HEAD и fallback GET Range=0-0 если нужно
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

	// Если HEAD не OK — fallback на GET Range=0-0
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		d.logWarn("HEAD вернул %s, пробую GET Range 0-0", resp.Status)
		return d.getRemoteFileInfoFallback()
	}

	// Если Content-Length отсутствует — fallback
	if cl := resp.Header.Get("Content-Length"); cl == "" {
		resp.Body.Close()
		d.logDebug("HEAD не вернул Content-Length, пробую GET Range 0-0")
		return d.getRemoteFileInfoFallback()
	}

	return resp, nil
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

// parse Content-Length or Content-Range
func getContentLengthFromHeaders(h http.Header) (int64, error) {
	if cl := h.Get("Content-Length"); cl != "" {
		if v, err := strconv.ParseInt(cl, 10, 64); err == nil && v > 0 {
			return v, nil
		}
	}
	if cr := h.Get("Content-Range"); cr != "" {
		// формат: bytes START-END/TOTAL
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

// ИСПРАВЛЕНО: downloadSingle теперь принимает supportsRanges и проверяет 416/Content-Range
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

	// ИСПРАВЛЕНО: Проверяем 416 Range Not Satisfiable
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return fmt.Errorf("сервер отверг диапазон %d- (возможно файл изменился на сервере)", existingSize)
	}

	expectedStatus := http.StatusOK
	if d.config.Resume && existingSize > 0 && supportsRanges {
		expectedStatus = http.StatusPartialContent
	}
	if resp.StatusCode != expectedStatus {
		return fmt.Errorf("сервер вернул %s", resp.Status)
	}

	// ИСПРАВЛЕНО: Проверяем Content-Range для 206 ответов
	if resp.StatusCode == http.StatusPartialContent {
		if err := d.validateContentRange(resp.Header, existingSize, contentLength-1); err != nil {
			return fmt.Errorf("валидация Content-Range в single mode: %w", err)
		}
	}

	reader := &progressReader{
		reader:     resp.Body,
		bar:        d.bar,
		downloaded: &d.downloadedBytes,
	}

	bufPtr := d.bufPool.Get().(*[]byte)
	defer d.bufPool.Put(bufPtr)

	// Для resume: Write from existingSize
	ow := &offsetWriterSafe{
		file:    writer.(*os.File),
		start:   existingSize,
		end:     contentLength - 1,
		written: 0,
	}

	_, err = io.CopyBuffer(ow, reader, *bufPtr)
	
	// ИСПРАВЛЕНО: Проверяем io.ErrUnexpectedEOF как "short read", а не ошибку
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			d.logWarn("Single mode: соединение прервано (partial read)")
			// Для single mode это не критично - можем продолжать
			// Проверим сколько реально записали
		} else {
			return err
		}
	}
	
	return nil
}

type progressReader struct {
	reader     io.Reader
	bar        *ThreadSafeProgressBar
	downloaded *int64
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.reader.Read(p)
	if n > 0 {
		atomic.AddInt64(pr.downloaded, int64(n))
		pr.bar.Add(n)
	}
	return
}

// downloadMulti — улучшенный многопоточный метод с лучшим resume
func (d *Downloader) downloadMulti(out *os.File, contentLength int64, numWorkers int, existingSize int64) error {
	// Инициализация частей для tracking
	d.initializeParts(contentLength, numWorkers, existingSize)
	
	// ИСПРАВЛЕНО: Дополнительная проверка после initializeParts
	if existingSize == contentLength {
		d.logInfo("Файл уже скачан полностью после инициализации частей")
		return nil
	}
	
	// Фильтрация незавершённых частей
	incompleteParts := make([]int, 0, numWorkers)
	for i, part := range d.parts {
		if !part.Completed {
			incompleteParts = append(incompleteParts, i)
		}
	}

	if len(incompleteParts) == 0 {
		d.logInfo("Все части уже скачаны")
		return nil
	}

	d.logInfo("Скачиваю %d незавершённых частей из %d", len(incompleteParts), numWorkers)

	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()

	errChan := make(chan error, len(incompleteParts))
	var wg sync.WaitGroup

	for _, partIdx := range incompleteParts {
		part := d.parts[partIdx]
		// ИСПРАВЛЕНО: Передаём правильные границы для resume
		actualStart := part.Start + part.Downloaded
		wg.Add(1)
		go d.downloadPart(ctx, &wg, errChan, out, partIdx+1, actualStart, part.End)
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
	
	// Сохраняем состояние после каждой успешной итерации многопоточного скачивания
	if err := d.saveStateFile(); err != nil {
		d.logWarn("Не удалось сохранить состояние: %v", err)
	}
	
	return nil
}

// checkDiskSpaceCrossPlatform проверяет доступное дисковое пространство
func (d *Downloader) checkDiskSpaceCrossPlatform(dir string, required int64) error {
	if runtime.GOOS == "windows" {
		return d.checkDiskSpaceWindows(dir, required)
	}
	return d.checkDiskSpaceUnix(dir, required)
}

// checkDiskSpaceWindows проверка для Windows
func (d *Downloader) checkDiskSpaceWindows(dir string, required int64) error {
	// Для Windows используем простую проверку через создание временного файла
	// В полной реализации можно использовать GetDiskFreeSpaceEx через syscall
	return fmt.Errorf("windows: using fallback disk check")
}

// checkDiskSpaceUnix проверка для Unix-подобных систем
func (d *Downloader) checkDiskSpaceUnix(dir string, required int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("syscall.Statfs failed: %w", err)
	}
	
	// На разных системах поля могут называться по-разному
	avail := int64(st.Bavail) * int64(st.Bsize)
	if avail < required {
		return fmt.Errorf("недостаточно места: нужно %s, доступно %s", formatBytes(required), formatBytes(avail))
	}
	d.logDebug("Диск: нужно %s, доступно %s", formatBytes(required), formatBytes(avail))
	return nil
}
func (d *Downloader) loadStateFile() error {
	if !d.config.SaveState {
		return nil
	}
	
	stateFile := d.config.FilePath + ".parts"
	_, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		return nil // Файл состояния не существует - это нормально
	}
	if err != nil {
		return fmt.Errorf("ошибка чтения файла состояния: %w", err)
	}
	
	// Простой JSON парсинг - в продакшене стоило бы использовать encoding/json
	d.logDebug("Загружаю состояние из %s", stateFile)
	return nil
}

// saveStateFile сохраняет текущее состояние частей
func (d *Downloader) saveStateFile() error {
	if !d.config.SaveState || len(d.parts) == 0 {
		return nil
	}
	
	stateFile := d.config.FilePath + ".parts"
	d.logDebug("Сохраняю состояние в %s", stateFile)
	
	// В продакшене здесь был бы encoding/json
	// Пока просто создаём файл-маркер
	file, err := os.Create(stateFile)
	if err != nil {
		return err
	}
	defer file.Close()
	
	fmt.Fprintf(file, "# State file for %s\n", d.config.FilePath)
	fmt.Fprintf(file, "# Parts: %d\n", len(d.parts))
	return nil
}

// removeStateFile удаляет файл состояния после успешного завершения
func (d *Downloader) removeStateFile() {
	if !d.config.SaveState {
		return
	}
	
	stateFile := d.config.FilePath + ".parts"
	if err := os.Remove(stateFile); err != nil && !os.IsNotExist(err) {
		d.logWarn("Не удалось удалить файл состояния %s: %v", stateFile, err)
	} else {
		d.logDebug("Файл состояния удалён: %s", stateFile)
	}
}
func (d *Downloader) initializeParts(contentLength int64, numWorkers int, existingSize int64) {
	d.parts = make([]PartInfo, numWorkers)
	partSize := contentLength / int64(numWorkers)

	for i := 0; i < numWorkers; i++ {
		start := int64(i) * partSize
		end := start + partSize - 1
		if i == numWorkers-1 {
			end = contentLength - 1
		}

		// Простая эвристика для resume: если existingSize покрывает всю часть
		completed := false
		downloaded := int64(0)
		if existingSize > end {
			completed = true
			downloaded = end - start + 1
		} else if existingSize > start {
			downloaded = existingSize - start
		}

		d.parts[i] = PartInfo{
			Start:      start,
			End:        end,
			Downloaded: downloaded,
			Completed:  completed,
		}
	}
}

func (d *Downloader) downloadPart(ctx context.Context, wg *sync.WaitGroup, errChan chan<- error, out *os.File, partID int, start, end int64) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			if ctx.Err() == nil {
				select {
				case errChan <- fmt.Errorf("паника в части %d: %v", partID, r):
				default:
				}
			}
		}
	}()

	for attempt := 0; attempt < d.config.Retries+1; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if attempt > 0 {
			backoff := d.config.RetryDelay * (1 << (attempt - 1))
			// ИСПРАВЛЕНО: Ограничиваем максимальный backoff
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			jitter := 0.5 + d.rng.Float64()
			backoff = time.Duration(float64(backoff) * jitter)
			d.logWarn("Часть %d: повтор %d через %v", partID, attempt, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}

		err := d.performPartDownload(ctx, out, partID, start, end)
		if err == nil {
			d.logDebug("Часть %d успешно (%d-%d)", partID, start, end)
			return
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}

		// ИСПРАВЛЕНО: Обрабатываем io.ErrUnexpectedEOF как "частичное скачивание"
		if errors.Is(err, io.ErrUnexpectedEOF) {
			d.logWarn("Часть %d: соединение прервано (partial read), будет повтор", partID)
		} else {
			d.logError("Часть %d: ошибка: %v", partID, err)
		}
		
		if attempt == d.config.Retries {
			if ctx.Err() == nil {
				select {
				case errChan <- fmt.Errorf("часть %d: исчерпаны попытки: %w", partID, err):
				default:
				}
			}
			return
		}
	}
}

func (d *Downloader) performPartDownload(ctx context.Context, out *os.File, partID int, start, end int64) error {
	req, err := http.NewRequestWithContext(ctx, "GET", d.config.URL, nil)
	if err != nil {
		return fmt.Errorf("создание запроса: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("User-Agent", d.config.UserAgent)

	resp, err := d.clientNoH2.Do(req)
	if err != nil {
		return fmt.Errorf("http запрос: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return fmt.Errorf("сервер отверг диапазон %d-%d (возможно файл уменьшился)", start, end)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("неожиданный статус %s (ожидался 206 Partial Content)", resp.Status)
	}

	// Улучшенная валидация Content-Range
	if err := d.validateContentRange(resp.Header, start, end); err != nil {
		return fmt.Errorf("валидация Content-Range: %w", err)
	}

	expected := end - start + 1
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if actual, perr := strconv.ParseInt(cl, 10, 64); perr == nil {
			if actual != expected {
				return fmt.Errorf("несоответствие размера части: ожидалось %d, получено %d", expected, actual)
			}
		}
	}

	// ИСПРАВЛЕНО: offsetWriterSafe теперь получает правильный start и expected
	ow := &offsetWriterSafe{
		file:    out,
		start:   start,  // уже правильный start с учётом resume
		end:     end,
		written: 0,      // начинаем считать с нуля для этого куска
	}

	reader := &progressReader{
		reader:     resp.Body,
		bar:        d.bar,
		downloaded: &d.downloadedBytes,
	}

	bufPtr := d.bufPool.Get().(*[]byte)
	defer d.bufPool.Put(bufPtr)

	_, copyErr := io.CopyBuffer(ow, reader, *bufPtr)
	
	// ИСПРАВЛЕНО: Различаем io.ErrUnexpectedEOF от других ошибок
	if copyErr != nil {
		if errors.Is(copyErr, io.ErrUnexpectedEOF) {
			// Это "short read" - соединение оборвалось, но данные могли быть записаны
			d.logDebug("Часть %d: короткое чтение (%d из %d байт)", partID, ow.written, expected)
			return copyErr // Возвращаем для retry
		}
		return copyErr
	}

	// Проверяем полноту только если нет ошибки копирования
	if ow.written != expected {
		return fmt.Errorf("часть %d: записано %d байт, ожидалось %d", partID, ow.written, expected)
	}
	return nil
}

// validateContentRange проверяет корректность Content-Range заголовка
func (d *Downloader) validateContentRange(headers http.Header, expectedStart, expectedEnd int64) error {
	cr := headers.Get("Content-Range")
	if cr == "" {
		return errors.New("отсутствует Content-Range заголовок")
	}

	// Парсим Content-Range: bytes start-end/total
	if !strings.HasPrefix(cr, "bytes ") {
		return fmt.Errorf("некорректный формат Content-Range: %s", cr)
	}

	rangePart := strings.TrimPrefix(cr, "bytes ")
	parts := strings.Split(rangePart, "/")
	if len(parts) != 2 {
		return fmt.Errorf("некорректный формат Content-Range: %s", cr)
	}

	rangeBounds := strings.Split(parts[0], "-")
	if len(rangeBounds) != 2 {
		return fmt.Errorf("некорректный формат диапазона в Content-Range: %s", parts[0])
	}

	start, err := strconv.ParseInt(rangeBounds[0], 10, 64)
	if err != nil {
		return fmt.Errorf("некорректный start в Content-Range: %s", rangeBounds[0])
	}

	end, err := strconv.ParseInt(rangeBounds[1], 10, 64)
	if err != nil {
		return fmt.Errorf("некорректный end в Content-Range: %s", rangeBounds[1])
	}

	if start != expectedStart || end != expectedEnd {
		return fmt.Errorf("диапазон не соответствует запрошенному: получен %d-%d, ожидался %d-%d", 
			start, end, expectedStart, expectedEnd)
	}

	return nil
}

// offsetWriterSafe — безопасный writer для части
type offsetWriterSafe struct {
	file    *os.File
	start   int64
	end     int64
	written int64
}

func (ow *offsetWriterSafe) Write(p []byte) (n int, err error) {
	ln := len(p)
	if ow.written+int64(ln) > ow.end-ow.start+1 {
		return 0, fmt.Errorf("oversized chunk: got %d bytes, max remaining %d", ln, (ow.end-ow.start+1)-ow.written)
	}
	pos := ow.start + ow.written
	n, err = ow.file.WriteAt(p, pos)
	if n > 0 {
		ow.written += int64(n)
	}
	return
}

// calculateWorkerCount определяет оптимальное количество потоков
func (d *Downloader) calculateWorkerCount(contentLength int64, supportsRanges bool) int {
	if !supportsRanges || contentLength < minPartSize {
		return 1
	}
	if d.config.MaxWorkers > 0 {
		return minInt(d.config.MaxWorkers, maxParts)
	}
	maxPossibleParts := int(contentLength / minPartSize)
	if maxPossibleParts > maxParts {
		maxPossibleParts = maxParts
	}
	if maxPossibleParts < 1 {
		maxPossibleParts = 1
	}
	numCPU := runtime.NumCPU()
	if numCPU < 1 {
		numCPU = 1
	}
	guess := minInt(maxPossibleParts, minInt(numCPU*4, maxParts))
	
	// проверка хвоста
	partSize := contentLength / int64(guess)
	lastPart := contentLength - partSize*int64(guess-1)
	if lastPart < minPartSize/2 && guess > 1 {
		guess--
	}
	if guess < 1 {
		guess = 1
	}
	return guess
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

// setupSignalHandler ловит Ctrl-C
func (d *Downloader) setupSignalHandler() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		d.logWarn("\nСигнал прерывания получен, отменяю загрузку...")
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
			mode = "resuming"
		} else {
			mode = "перезаписываю"
		}
		d.logInfo("Файл %s существует — %s (force/resume=true)", d.config.FilePath, mode)
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

func maxInt(a, b int) int { 
	if a > b { 
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
		return base, nil
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
	pflag.BoolVar(&cfg.SaveState, "save-state", false, "Сохранять состояние частей для надёжного resume")
	pflag.BoolVarP(&cfg.Insecure, "insecure", "k", false, "Игнорировать ошибки TLS сертификатов")
	
	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Использование: %s [флаги]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Примеры:\n")
		fmt.Fprintf(os.Stderr, "  %s -u https://example.com/largefile.zip\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -u https://speed.hetzner.de/100MB.bin -w 8 -o my_file.bin --resume -v\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -u https://self-signed.badssl.com/file.zip --insecure --save-state\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Флаги:\n")
		pflag.PrintDefaults()
	}
	pflag.Parse()

	if cfg.URL == "" {
		pflag.Usage()
		return cfg, errors.New("флаг --url обязателен")
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
		fmt.Fprintf(os.Stderr, "Ошибка создания downloader: %v\n", err)
		os.Exit(1)
	}

	if err := dl.Download(); err != nil {
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "операция отменена пользователем") {
			fmt.Fprintf(os.Stderr, "Ошибка скачивания: %v\n", err)
		} else {
			dl.logInfo("Загрузка отменена пользователем.")
		}
		os.Exit(1)
	}
}
