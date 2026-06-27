package pload

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

const (
	maxParts          = 64
	minPartSize       = 2 * 1024 * 1024
	defaultBufSize    = 256 * 1024
	maxBufferSize     = 8 * 1024 * 1024
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
	Color      string
	Quiet      bool
}

func (c *Config) Validate() error {
	if c.URL == "" {
		return errors.New("URL cannot be empty")
	}
	if c.MaxWorkers < 0 {
		return errors.New("worker count cannot be negative")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if c.BufferSize <= 0 {
		return errors.New("buffer-size must be positive")
	}
	if c.BufferSize > maxBufferSize {
		return fmt.Errorf("buffer-size too large (max %d)", maxBufferSize)
	}
	if c.Retries < 0 {
		return errors.New("retries cannot be negative")
	}
	if c.RetryDelay <= 0 {
		return errors.New("retry-delay must be positive")
	}
	switch c.Color {
	case "auto", "always", "never", "":
	default:
		return fmt.Errorf("invalid --color value: %s (must be auto/always/never)", c.Color)
	}
	return nil
}

func ParseFlags() (Config, error) {
	var cfg Config
	var logLevelStr string

	pflag.StringVarP(&cfg.URL, "url", "u", "", "Download URL (required)")
	pflag.StringVarP(&cfg.FilePath, "output", "o", "", "Output file path (default: filename from URL or Content-Disposition)")
	pflag.IntVarP(&cfg.MaxWorkers, "workers", "w", 0, "Number of workers (0 = auto)")
	pflag.DurationVarP(&cfg.Timeout, "timeout", "t", defaultTimeout, "HTTP request timeout")
	pflag.StringVarP(&cfg.UserAgent, "user-agent", "a", "", "User-Agent header")
	pflag.BoolVarP(&cfg.Overwrite, "force", "f", false, "Overwrite without confirmation")
	pflag.BoolVarP(&cfg.Verbose, "verbose", "v", false, "Verbose output (implies --log-level=debug)")
	pflag.StringVar(&logLevelStr, "log-level", "info", "Log level: error, warn, info, debug")
	pflag.IntVar(&cfg.BufferSize, "buffer-size", defaultBufSize, "Buffer size in bytes (max 8 MiB)")
	pflag.BoolVar(&cfg.NoEmoji, "no-emoji", false, "Disable emoji in logs")
	pflag.IntVar(&cfg.Retries, "retries", defaultRetries, "Number of retries per part")
	pflag.DurationVar(&cfg.RetryDelay, "retry-delay", defaultRetryDelay, "Base retry delay")
	pflag.BoolVar(&cfg.Resume, "resume", false, "Resume partial download if possible")
	pflag.BoolVar(&cfg.SaveState, "save-state", true, "Save part state for reliable resume (enabled by default)")
	pflag.BoolVarP(&cfg.Insecure, "insecure", "k", false, "Skip TLS certificate verification")
	pflag.StringVar(&cfg.Color, "color", "auto", "Color output: auto, always, never")
	pflag.BoolVarP(&cfg.Quiet, "quiet", "q", false, "Quiet mode: errors only + one-line summary")

	pflag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <URL>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  %s https://example.com/largefile.zip\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -w 8 -o my_file.bin --resume -v https://speed.hetzner.de/100MB.bin\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s --insecure https://self-signed.badssl.com/file.zip\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -q https://example.com/file.zip\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nFlags:\n")
		pflag.PrintDefaults()
	}
	pflag.Parse()

	for _, arg := range os.Args[1:] {
		if arg == "-h" || arg == "--help" {
			pflag.Usage()
			os.Exit(0)
		}
	}

	if cfg.Resume {
		cfg.SaveState = true
	}

	if cfg.URL == "" {
		if pflag.NArg() > 0 {
			cfg.URL = pflag.Arg(0)
		} else {
			pflag.Usage()
			return cfg, errors.New("--url flag or positional URL argument is required")
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
			return cfg, fmt.Errorf("invalid --log-level: %s", logLevelStr)
		}
	}

	return cfg, nil
}
