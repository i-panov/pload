package pload

import (
	"net/http"
	"testing"
	"time"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{500, "500 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{1073741824, "1.0 GiB"},
		{1099511627776, "1.0 TiB"},
	}

	for _, tt := range tests {
		got := formatBytes(tt.input)
		if got != tt.want {
			t.Errorf("formatBytes(%d) = %q; want %q", tt.input, got, tt.want)
		}
	}
}

func TestBasenameFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
		err  bool
	}{
		{"https://example.com/file.zip", "file.zip", false},
		{"https://example.com/path/to/file.tar.gz", "file.tar.gz", false},
		{"https://example.com/file%20name.txt", "file name.txt", false},
		{"https://example.com/", "", true},
		{"https://example.com/%D1%84%D0%B0%D0%B9%D0%BB.zip", "файл.zip", false},
	}

	for _, tt := range tests {
		got, err := basenameFromURL(tt.url)
		if tt.err {
			if err == nil {
				t.Errorf("basenameFromURL(%q) expected error, got %q", tt.url, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("basenameFromURL(%q) unexpected error: %v", tt.url, err)
			continue
		}
		if got != tt.want {
			t.Errorf("basenameFromURL(%q) = %q; want %q", tt.url, got, tt.want)
		}
	}
}

func TestGetContentLengthFromHeaders(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		want    int64
		err     bool
	}{
		{
			name: "Content-Length present",
			headers: http.Header{
				"Content-Length": {"1048576"},
			},
			want: 1048576,
			err:  false,
		},
		{
			name: "Content-Range present",
			headers: http.Header{
				"Content-Range": {"bytes 0-100/5000"},
			},
			want: 5000,
			err:  false,
		},
		{
			name: "Content-Range with wildcard",
			headers: http.Header{
				"Content-Range": {"bytes */5000"},
			},
			want: 5000,
			err:  false,
		},
		{
			name: "no headers",
			headers: http.Header{},
			want: 0,
			err:  true,
		},
		{
			name: "invalid Content-Length",
			headers: http.Header{
				"Content-Length": {"not-a-number"},
			},
			want: 0,
			err:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getContentLengthFromHeaders(tt.headers)
			if tt.err {
				if err == nil {
					t.Errorf("expected error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("got %d; want %d", got, tt.want)
			}
		})
	}
}

func TestParseContentDisposition(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		want    string
	}{
		{
			name: "simple filename",
			headers: http.Header{
				"Content-Disposition": {`attachment; filename="file.zip"`},
			},
			want: "file.zip",
		},
		{
			name: "utf-8 encoded",
			headers: http.Header{
				"Content-Disposition": {`attachment; filename*=UTF-8''%D1%84%D0%B0%D0%B9%D0%BB.zip`},
			},
			want: "файл.zip",
		},
		{
			name: "no header",
			headers: http.Header{},
			want: "",
		},
		{
			name: "no disposition",
			headers: http.Header{
				"Content-Type": {"text/plain"},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseContentDisposition(tt.headers)
			if got != tt.want {
				t.Errorf("got %q; want %q", got, tt.want)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		err  bool
	}{
		{
			name: "valid config",
			cfg: Config{
				URL:        "https://example.com/file.zip",
				Timeout:    30 * time.Second,
				BufferSize: 262144,
				Retries:    5,
				RetryDelay: 2 * time.Second,
			},
			err: false,
		},
		{
			name: "empty URL",
			cfg: Config{
				URL:        "",
				Timeout:    30 * time.Second,
				BufferSize: 262144,
				Retries:    5,
				RetryDelay: 2 * time.Second,
			},
			err: true,
		},
		{
			name: "negative workers",
			cfg: Config{
				URL:        "https://example.com/file.zip",
				MaxWorkers: -1,
				Timeout:    30 * time.Second,
				BufferSize: 262144,
				Retries:    5,
				RetryDelay: 2 * time.Second,
			},
			err: true,
		},
		{
			name: "zero timeout",
			cfg: Config{
				URL:        "https://example.com/file.zip",
				Timeout:    0,
				BufferSize: 262144,
				Retries:    5,
				RetryDelay: 2 * time.Second,
			},
			err: true,
		},
		{
			name: "buffer too large",
			cfg: Config{
				URL:        "https://example.com/file.zip",
				Timeout:    30 * time.Second,
				BufferSize: 16 * 1024 * 1024,
				Retries:    5,
				RetryDelay: 2 * time.Second,
			},
			err: true,
		},
		{
			name: "negative retries",
			cfg: Config{
				URL:        "https://example.com/file.zip",
				Timeout:    30 * time.Second,
				BufferSize: 262144,
				Retries:    -1,
				RetryDelay: 2 * time.Second,
			},
			err: true,
		},
		{
			name: "invalid color",
			cfg: Config{
				URL:        "https://example.com/file.zip",
				Timeout:    30 * time.Second,
				BufferSize: 262144,
				Retries:    5,
				RetryDelay: 2 * time.Second,
				Color:      "hotpink",
			},
			err: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.err {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestCalculateWorkerCount(t *testing.T) {
	d := &Downloader{
		config: Config{},
	}

	// With no MaxWorkers set, should use auto-detection
	// Large file, no range -> 1 worker
	if n := d.calculateWorkerCount(1024*1024*100, false); n != 1 {
		t.Errorf("expected 1 worker without ranges, got %d", n)
	}

	// Small file with ranges -> 1 worker (below minPartSize)
	if n := d.calculateWorkerCount(1024*1024, true); n != 1 {
		t.Errorf("expected 1 worker for small file, got %d", n)
	}

	// Large file with ranges and manual workers
	d.config.MaxWorkers = 4
	if n := d.calculateWorkerCount(1024*1024*100, true); n != 4 {
		t.Errorf("expected 4 workers, got %d", n)
	}

	// Even with manual workers, respect minPartSize
	d.config.MaxWorkers = 64
	if n := d.calculateWorkerCount(1024*1024*10, true); n != 5 {
		t.Errorf("expected 5 workers (10MB/2MB), got %d", n)
	}

	d.config.MaxWorkers = 0
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "0:30"},
		{90 * time.Second, "1:30"},
		{3600 * time.Second, "1:00:00"},
		{3661 * time.Second, "1:01:01"},
		{-1 * time.Second, "∞"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q; want %q", tt.d, got, tt.want)
		}
	}
}
