# pload

Multi-threaded file downloader with resume support.

## Features

- Multi-threaded download with automatic worker count detection
- Resume interrupted downloads (--resume)
- Disk space check before download
- HTTP/HTTPS with optional TLS skip (--insecure)
- Part state persistence for reliable resume (enabled by default)
- Automatic retries with exponential backoff + jitter
- Content-Disposition filename extraction
- Progress bar with real-time speed and ETA
- Color output (auto-detected, can be forced on/off with --color)
- Quiet mode for scripting (--quiet)

## Install

```bash
go build -o bin/pload .
# or
go install github.com/i-panov/pload@latest
```

## Usage

```bash
# Simple download
pload https://example.com/largefile.zip

# Output to specific file
pload -o myfile.zip https://example.com/largefile.zip

# 8 workers
pload -w 8 https://example.com/largefile.zip

# Resume interrupted download
pload --resume https://example.com/largefile.zip

# Verbose debug output
pload -v https://example.com/largefile.zip

# Skip TLS verification
pload --insecure https://self-signed.badssl.com/file.zip

# Quiet mode (one-line summary)
pload -q https://example.com/file.zip

# Disable colors
pload --color=never https://example.com/file.zip
```

## Build

```bash
make build      # builds to bin/pload
make test       # run tests
make clean      # remove bin/
```

## Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--url` | `-u` | | Download URL (required) |
| `--output` | `-o` | auto¹ | Output file path |
| `--workers` | `-w` | auto | Number of workers (0 = auto) |
| `--timeout` | `-t` | 1m0s | HTTP request timeout |
| `--user-agent` | `-a` | Go-MultiThread-Downloader/3.3.0 | User-Agent header |
| `--force` | `-f` | false | Overwrite without confirmation |
| `--verbose` | `-v` | false | Verbose output (implies --log-level=debug) |
| `--log-level` | | info | Log level: error, warn, info, debug |
| `--buffer-size` | | 256 KiB | Buffer size in bytes (max 8 MiB) |
| `--no-emoji` | | false | Disable emoji in logs |
| `--retries` | | 5 | Retries per part |
| `--retry-delay` | | 2s | Base retry delay |
| `--resume` | | false | Resume partial download |
| `--save-state` | | true | Save part state (always on with --resume) |
| `--insecure` | `-k` | false | Skip TLS verification |
| `--color` | | auto | Color output: auto, always, never |
| `--quiet` | `-q` | false | Errors only + one-line completion summary |

¹ Priority: Content-Disposition → URL basename → `downloaded_file`

## How it works

1. Sends HEAD request to get file info (Content-Length, Accept-Ranges, Content-Disposition)
2. If HEAD fails, falls back to GET with Range: bytes=0-0
3. Checks available disk space
4. Pre-allocates the file (sparse if supported by filesystem)
5. Splits file into parts, downloads them in parallel
6. Each part retries on failure with exponential backoff + jitter
7. Saves part state for resume after each completed part
8. Verifies final file size on completion

## License

BSD 3-Clause License
