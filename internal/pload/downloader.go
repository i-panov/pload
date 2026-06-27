package pload

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"
)

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
	partsMu         sync.Mutex
	parts           []PartInfo
	startTime       time.Time
	totalRetries    int64
	numWorkers      int
}

func NewDownloader(cfg Config) (*Downloader, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

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
	if cfg.Color == "" {
		cfg.Color = "auto"
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
	noH2Transport.TLSClientConfig = &tls.Config{
		NextProtos: []string{"http/1.1"},
	}

	if cfg.Insecure {
		defaultTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		noH2Transport.TLSClientConfig.InsecureSkipVerify = true
		fmt.Fprintf(os.Stderr, "Warning: TLS certificate verification disabled (--insecure)\n")
	}

	pool := &sync.Pool{
		New: func() any {
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
	d.startTime = time.Now()

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
		if d.bar != nil {
			_ = d.bar.Finish()
		}
		if err != nil && !errors.Is(err, context.Canceled) && !d.config.SaveState {
			d.cleanupPartialFile()
		}
	}()

	contentLength, supportsRanges, cdFilename, err := d.getRemoteFileInfo()
	if err != nil {
		return err
	}

	if d.config.FilePath == "" {
		if cdFilename != "" {
			d.config.FilePath = cdFilename
		} else {
			if name, e := basenameFromURL(d.config.URL); e == nil {
				d.config.FilePath = name
			} else {
				d.config.FilePath = "downloaded_file"
			}
		}
	}

	numWorkers := d.calculateWorkerCount(contentLength, supportsRanges)
	d.numWorkers = numWorkers
	d.logInfo("Info: size %s, workers %d", formatBytes(contentLength), numWorkers)

	if !supportsRanges {
		d.logWarn("Server does not support Range requests — falling back to single-threaded download")
		d.numWorkers = 1
	}

	d.printDownloadInfo(contentLength, d.numWorkers, supportsRanges)

	if err := d.prepareLocalFile(contentLength); err != nil {
		return err
	}

	d.initializeParts(contentLength, numWorkers)
	if d.config.Resume {
		if err := d.loadStateFile(); err != nil {
			d.logWarn("Failed to load state for resume: %v", err)
		}
	}

	initialDownloaded := d.calculateInitialDownloaded()
	if initialDownloaded == contentLength {
		d.logInfo("File already fully downloaded (%s), skipping", formatBytes(contentLength))
		d.removeStateFile()
		return nil
	}

	out, err := os.OpenFile(d.config.FilePath, os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("cannot open file for writing: %w", err)
	}

	d.bar = &ThreadSafeProgressBar{bar: progressbar.DefaultBytes(contentLength, "downloading")}
	if initialDownloaded > 0 {
		d.bar.Add64(initialDownloaded)
		atomic.StoreInt64(&d.downloadedBytes, initialDownloaded)
	}

	downloadCtx, downloadCancel := context.WithCancel(d.ctx)
	defer downloadCancel()

	d.startSpeedTracker(downloadCtx, contentLength)

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
		return fmt.Errorf("error closing file: %w", closeErr)
	}

	finalStat, statErr := os.Stat(d.config.FilePath)
	if statErr != nil {
		return fmt.Errorf("error checking final size: %w", statErr)
	}

	if finalStat.Size() != contentLength {
		return fmt.Errorf("file corrupted: expected %d, got %d", contentLength, finalStat.Size())
	}

	d.printCompletionStats(contentLength)
	d.removeStateFile()
	return nil
}

func (d *Downloader) calculateWorkerCount(contentLength int64, supportsRanges bool) int {
	if !supportsRanges || contentLength < minPartSize {
		return 1
	}

	if d.config.MaxWorkers > 0 {
		w := d.config.MaxWorkers
		if contentLength/int64(w) < minPartSize {
			w = int(contentLength / minPartSize)
		}
		if w < 1 {
			w = 1
		}
		return min(w, maxParts)
	}

	numCPU := max(runtime.NumCPU(), 1)
	guess := min(numCPU*4, maxParts)

	if contentLength/int64(guess) < minPartSize {
		guess = int(contentLength / minPartSize)
	}

	if guess < 1 {
		return 1
	}
	return min(guess, maxParts)
}

func (d *Downloader) printDownloadInfo(contentLength int64, workers int, supportsRanges bool) {
	if d.config.Quiet {
		return
	}

	mode := "New download"
	if d.config.Resume {
		mode = "Resume"
	}
	rangeSupport := "Yes"
	if !supportsRanges {
		rangeSupport = "No (single thread)"
	}

	fmt.Fprintf(os.Stderr, "\n")
	d.printLine("URL", d.config.URL)
	d.printLine("File", d.config.FilePath)
	d.printLine("Size", formatBytes(contentLength))
	d.printLine("Workers", fmt.Sprintf("%d", workers))
	d.printLine("Range", rangeSupport)
	d.printLine("Mode", mode)
	fmt.Fprintf(os.Stderr, "\n")
}

func (d *Downloader) printLine(label, value string) {
	if d.config.Quiet {
		return
	}
	fmt.Fprintf(os.Stderr, "  %-12s %s\n", label+":", value)
}

func (d *Downloader) printCompletionStats(contentLength int64) {
	if d.config.Quiet {
		elapsed := time.Since(d.startTime)
		fmt.Fprintf(os.Stderr, "✔ %s  %s  %s  %s/s\n",
			d.config.FilePath,
			formatBytes(contentLength),
			formatDuration(elapsed),
			formatBytes(int64(float64(contentLength)/elapsed.Seconds())),
		)
		return
	}

	elapsed := time.Since(d.startTime)
	avgSpeed := int64(float64(contentLength) / elapsed.Seconds())

	fmt.Fprintf(os.Stderr, "\n")
	d.printLine("File", d.config.FilePath)
	d.printLine("Size", formatBytes(contentLength))
	d.printLine("Time", formatDuration(elapsed))
	d.printLine("Avg speed", formatBytes(avgSpeed)+"/s")
	d.printLine("Workers", fmt.Sprintf("%d", d.numWorkers))
	retries := atomic.LoadInt64(&d.totalRetries)
	if retries > 0 {
		d.printLine("Retries", fmt.Sprintf("%d", retries))
	}
	fmt.Fprintf(os.Stderr, "\n")
}
