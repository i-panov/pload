package pload

import (
	"fmt"
	"os"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
)

func (d *Downloader) logDebug(format string, args ...interface{}) {
	if d.config.LogLevel >= LogDebug {
		d.logWithColor(colorGray, " ", format, args...)
	}
}

func (d *Downloader) LogInfo(format string, args ...interface{}) {
	if d.config.LogLevel >= LogInfo {
		d.logWithColor(colorCyan, " ", format, args...)
	}
}

func (d *Downloader) logInfo(format string, args ...interface{}) {
	d.LogInfo(format, args...)
}

func (d *Downloader) logWarn(format string, args ...interface{}) {
	if d.config.LogLevel >= LogWarn {
		d.logWithColor(colorYellow, " ", format, args...)
	}
}

func (d *Downloader) LogError(format string, args ...interface{}) {
	if d.config.LogLevel >= LogError {
		d.logWithColor(colorRed, " ", format, args...)
	}
}

func (d *Downloader) logError(format string, args ...interface{}) {
	d.LogError(format, args...)
}

func (d *Downloader) logWithColor(color, prefixEmoji, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)

	if d.shouldUseColor() {
		fmt.Fprintf(os.Stderr, "%s%s%s\n", color, msg, colorReset)
	} else if d.config.NoEmoji {
		fmt.Fprintf(os.Stderr, "%s\n", msg)
	} else {
		fmt.Fprintf(os.Stderr, "%s%s\n", prefixEmoji, msg)
	}
}

func (d *Downloader) shouldUseColor() bool {
	switch d.config.Color {
	case "always":
		return true
	case "never":
		return false
	default:
		fi, _ := os.Stderr.Stat()
		return (fi.Mode() & os.ModeCharDevice) != 0
	}
}
