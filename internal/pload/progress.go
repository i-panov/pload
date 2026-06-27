package pload

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"
)

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

func (d *Downloader) startSpeedTracker(ctx context.Context, contentLength int64) {
	if d.config.Quiet {
		return
	}

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		var prev int64

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				current := atomic.LoadInt64(&d.downloadedBytes)
				speed := current - prev
				prev = current

				if speed > 0 {
					remaining := contentLength - current
					eta := time.Duration(float64(remaining)/float64(speed)) * time.Second
					etaStr := formatDuration(eta)

					d.bar.mu.Lock()
					d.bar.bar.Describe(fmt.Sprintf("%s/s | ETA %s", formatBytes(speed), etaStr))
					d.bar.mu.Unlock()
				}
			}
		}
	}()
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		return "∞"
	}
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func (d *Downloader) startSpinner(ctx context.Context, msg string) {
	if d.config.Quiet {
		return
	}

	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	i := 0

	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				fmt.Fprintf(os.Stderr, "\r%s... done\n", msg)
				return
			case <-ticker.C:
				fmt.Fprintf(os.Stderr, "\r%s %s ", msg, frames[i%len(frames)])
				i++
			}
		}
	}()
}
