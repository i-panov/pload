package pload

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func (d *Downloader) prepareLocalFile(contentLength int64) error {
	if err := d.checkFileExists(); err != nil {
		return err
	}
	if err := d.checkDiskSpace(contentLength); err != nil {
		return err
	}

	stat, err := os.Stat(d.config.FilePath)
	if os.IsNotExist(err) || (!d.config.Resume && stat.Size() != contentLength) {
		d.logInfo("Creating file and allocating disk space...")
		out, createErr := os.Create(d.config.FilePath)
		if createErr != nil {
			return fmt.Errorf("creating file: %w", createErr)
		}
		if truncErr := out.Truncate(contentLength); truncErr != nil {
			out.Close()
			return fmt.Errorf("allocating space: %w", truncErr)
		}
		out.Close()
	} else if err == nil && d.config.Resume {
		d.logInfo("Resuming download of %s", d.config.FilePath)
	}

	return nil
}

func (d *Downloader) checkFileExists() error {
	_, err := os.Stat(d.config.FilePath)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("checking file: %w", err)
	}

	if d.config.Overwrite || d.config.Resume {
		var mode string
		if d.config.Resume {
			mode = "resuming"
		} else {
			mode = "overwriting"
		}
		d.logInfo("File %s exists — %s", d.config.FilePath, mode)
		return nil
	}

	d.logWarn("File %s already exists.", d.config.FilePath)
	fmt.Print("Overwrite? (y/N): ")
	var response string
	_, err = fmt.Scanln(&response)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading response: %w", err)
	}
	response = strings.ToLower(strings.TrimSpace(response))
	if response != "y" && response != "yes" {
		return errors.New("operation cancelled by user")
	}
	return nil
}

func (d *Downloader) checkDiskSpace(required int64) error {
	dir := filepath.Dir(d.config.FilePath)
	if err := d.checkDiskSpacePlatform(dir, required); err == nil {
		return nil
	} else {
		d.logDebug("Direct disk check unavailable (%v), using fallback", err)
	}

	temp, err := os.CreateTemp(dir, "downloader_space_*")
	if err != nil {
		return fmt.Errorf("cannot check disk space (temp file): %w", err)
	}
	defer os.Remove(temp.Name())
	defer temp.Close()

	if err := temp.Truncate(required); err != nil {
		return fmt.Errorf("insufficient disk space for file %s", formatBytes(required))
	}
	return nil
}

func (d *Downloader) cleanupPartialFile() {
	if d.filePath == "" {
		return
	}
	if err := os.Remove(d.filePath); err != nil && !os.IsNotExist(err) {
		d.logError("Failed to remove partial file: %v", err)
	} else if err == nil {
		d.logInfo("Partial file removed: %s", d.filePath)
	}
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < 0 {
		return "0 B"
	}
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
		return "", errors.New("cannot determine filename from URL")
	}
	decoded, err := url.QueryUnescape(base)
	if err != nil {
		return base, nil
	}
	return decoded, nil
}
