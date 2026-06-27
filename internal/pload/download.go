package pload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type progressReader struct {
	reader          io.Reader
	bar             *ThreadSafeProgressBar
	downloadedBytes *int64
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.reader.Read(p)
	if n > 0 {
		pr.bar.Add(n)
		atomic.AddInt64(pr.downloadedBytes, int64(n))
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

func (d *Downloader) downloadSingle(writer io.WriterAt, contentLength int64, existingSize int64, supportsRanges bool) error {
	req, err := http.NewRequestWithContext(d.ctx, "GET", d.config.URL, nil)
	if err != nil {
		return fmt.Errorf("creating GET request: %w", err)
	}
	if d.config.Resume && existingSize > 0 && supportsRanges {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existingSize))
	}
	req.Header.Set("User-Agent", d.config.UserAgent)

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("GET request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		d.logInfo("File already fully downloaded (server returned 416).")
		return nil
	}

	expectedStatus := http.StatusOK
	if d.config.Resume && existingSize > 0 && supportsRanges {
		expectedStatus = http.StatusPartialContent
	}
	if resp.StatusCode != expectedStatus {
		return fmt.Errorf("server returned %s, expected %d", resp.Status, expectedStatus)
	}

	if resp.StatusCode == http.StatusPartialContent {
		cr := resp.Header.Get("Content-Range")
		if cr != "" {
			if parts := strings.Split(cr, "/"); len(parts) == 2 {
				totalStr := strings.TrimSpace(parts[1])
				if total, err := strconv.ParseInt(totalStr, 10, 64); err == nil {
					if total != contentLength {
						return fmt.Errorf("file changed on server: expected size %d, got %d", contentLength, total)
					}
				}
			}
		}
	}

	reader := &progressReader{reader: resp.Body, bar: d.bar, downloadedBytes: &d.downloadedBytes}
	bufPtr := d.bufPool.Get().(*[]byte)
	defer d.bufPool.Put(bufPtr)

	ow := &offsetWriter{writer: writer, offset: existingSize}
	_, err = io.CopyBuffer(ow, reader, *bufPtr)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("download incomplete: connection interrupted: %w", err)
		}
		return err
	}
	return nil
}

func (d *Downloader) downloadMulti(out *os.File) error {
	incompleteParts := make([]int, 0, len(d.parts))
	for i, part := range d.parts {
		if !part.Completed {
			incompleteParts = append(incompleteParts, i)
		}
	}

	if len(incompleteParts) == 0 {
		d.logInfo("All parts already downloaded (per state file)")
		return nil
	}

	d.logInfo("Downloading %d incomplete parts of %d", len(incompleteParts), len(d.parts))

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
			return fmt.Errorf("error downloading part: %w", e)
		}
	}
	return nil
}

func (d *Downloader) downloadPart(ctx context.Context, wg *sync.WaitGroup, errChan chan<- error, out *os.File, partIndex int) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			errChan <- fmt.Errorf("panic in part %d: %v", partIndex+1, r)
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

		if start > end {
			return
		}

		if attempt > 0 {
			atomic.AddInt64(&d.totalRetries, 1)
			backoff := d.config.RetryDelay * (1 << (attempt - 1))
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			jitter := 0.5 + d.rng.Float64()
			backoff = time.Duration(float64(backoff) * jitter)
			d.logWarn("Part %d: retry %d in %v", partIndex+1, attempt, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}

		written, err := d.performPartDownload(ctx, out, partIndex+1, start, end)

		d.partsMu.Lock()
		if written > 0 {
			d.parts[partIndex].Downloaded += written
		}

		if err == nil {
			d.parts[partIndex].Completed = true
			d.parts[partIndex].Downloaded = d.parts[partIndex].End - d.parts[partIndex].Start + 1

			if d.config.SaveState {
				if saveErr := d.saveStateFileUnsafe(); saveErr != nil {
					d.logWarn("Failed to save final state for part %d: %v", partIndex+1, saveErr)
				}
			}
			d.partsMu.Unlock()
			return
		}

		// Save partial progress on error
		if written > 0 && d.config.SaveState {
			if saveErr := d.saveStateFileUnsafe(); saveErr != nil {
				d.logWarn("Failed to save intermediate state: %v", saveErr)
			}
		}
		d.partsMu.Unlock()

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}

		if errors.Is(err, io.ErrUnexpectedEOF) {
			d.logWarn("Part %d: connection interrupted (partial download), retrying", partIndex+1)
		} else {
			d.logError("Part %d: error: %v", partIndex+1, err)
		}

		if attempt == d.config.Retries {
			errChan <- fmt.Errorf("part %d: exhausted retries: %w", partIndex+1, err)
			return
		}
	}
}
