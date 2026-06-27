package pload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func (d *Downloader) getRemoteFileInfo() (contentLength int64, supportsRanges bool, filename string, err error) {
	d.logInfo("Fetching file info...")

	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()

	if !d.config.Quiet {
		d.startSpinner(ctx, "Fetching file info")
	}

	time.Sleep(50 * time.Millisecond) // let spinner render first

	req, reqErr := http.NewRequestWithContext(d.ctx, "HEAD", d.config.URL, nil)
	if reqErr != nil {
		return 0, false, "", fmt.Errorf("creating HEAD request: %w", reqErr)
	}
	req.Header.Set("User-Agent", d.config.UserAgent)

	resp, reqErr := d.client.Do(req)
	if reqErr != nil {
		return 0, false, "", fmt.Errorf("HEAD request failed: %w", reqErr)
	}

	if resp.StatusCode == http.StatusOK && resp.Header.Get("Content-Length") != "" {
		cl, clErr := getContentLengthFromHeaders(resp.Header)
		if clErr != nil {
			resp.Body.Close()
			return 0, false, "", clErr
		}
		supportsRanges := strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")
		fn := parseContentDisposition(resp.Header)
		resp.Body.Close()
		return cl, supportsRanges, fn, nil
	}
	resp.Body.Close()

	d.logWarn("HEAD returned %s or missing Content-Length, trying GET Range 0-0", resp.Status)
	return d.getRemoteFileInfoFallback()
}

func (d *Downloader) getRemoteFileInfoFallback() (contentLength int64, supportsRanges bool, filename string, err error) {
	req, err := http.NewRequestWithContext(d.ctx, "GET", d.config.URL, nil)
	if err != nil {
		return 0, false, "", fmt.Errorf("creating GET fallback request: %w", err)
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", d.config.UserAgent)

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, false, "", fmt.Errorf("GET fallback failed: %w", err)
	}
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return 0, false, "", fmt.Errorf("fallback returned status %s", resp.Status)
	}

	cl, err := getContentLengthFromHeaders(resp.Header)
	if err != nil {
		resp.Body.Close()
		return 0, false, "", err
	}
	fn := parseContentDisposition(resp.Header)
	resp.Body.Close()
	return cl, false, fn, nil
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
	return 0, errors.New("Content-Length and Content-Range missing or invalid")
}

func parseContentDisposition(headers http.Header) string {
	cd := headers.Get("Content-Disposition")
	if cd == "" {
		return ""
	}

	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}

	if fn, ok := params["filename*"]; ok {
		return fn
	}
	if fn, ok := params["filename"]; ok {
		return fn
	}
	return ""
}

func (d *Downloader) performPartDownload(ctx context.Context, out *os.File, partID int, start, end int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", d.config.URL, nil)
	if err != nil {
		return 0, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("User-Agent", d.config.UserAgent)

	resp, err := d.clientNoH2.Do(req)
	if err != nil {
		return 0, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return 0, fmt.Errorf("server rejected range %d-%d", start, end)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("unexpected status %s (expected 206)", resp.Status)
	}

	if err := d.validateContentRange(resp.Header, start, end); err != nil {
		return 0, fmt.Errorf("Content-Range validation: %w", err)
	}

	expectedSize := end - start + 1
	reader := &progressReader{reader: resp.Body, bar: d.bar, downloadedBytes: &d.downloadedBytes}
	ow := &offsetWriter{writer: out, offset: start}
	bufPtr := d.bufPool.Get().(*[]byte)
	defer d.bufPool.Put(bufPtr)

	written, copyErr := io.CopyBuffer(ow, reader, *bufPtr)

	if copyErr != nil {
		if errors.Is(copyErr, io.ErrUnexpectedEOF) {
			d.logDebug("Part %d: short read (%d of %d bytes)", partID, written, expectedSize)
			return written, copyErr
		}
		return written, copyErr
	}

	if written != expectedSize {
		return written, fmt.Errorf("part %d: wrote %d bytes, expected %d", partID, written, expectedSize)
	}
	return written, nil
}

func (d *Downloader) validateContentRange(headers http.Header, expectedStart, expectedEnd int64) error {
	cr := headers.Get("Content-Range")
	if cr == "" {
		return errors.New("missing Content-Range header")
	}

	_, after, found := strings.Cut(cr, " ")
	if !found {
		return fmt.Errorf("invalid Content-Range format: %s", cr)
	}
	rangeAndTotal, _, found := strings.Cut(after, "/")
	if !found {
		return fmt.Errorf("invalid Content-Range format: %s", cr)
	}
	startStr, endStr, found := strings.Cut(rangeAndTotal, "-")
	if !found {
		return fmt.Errorf("invalid range format in Content-Range: %s", rangeAndTotal)
	}

	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid start in Content-Range: %s", startStr)
	}
	end, err := strconv.ParseInt(endStr, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid end in Content-Range: %s", endStr)
	}

	if start != expectedStart || end != expectedEnd {
		return fmt.Errorf("range mismatch: got %d-%d, expected %d-%d", start, end, expectedStart, expectedEnd)
	}
	return nil
}
