# pload — AGENTS.md

## Build & test

```bash
make build          # go build -o bin/pload ./cmd/pload/
make test           # go test -v -race -count=1 ./...  (no network needed)
make vet            # go vet ./...
make clean          # rm -rf bin/
```

Tests are pure unit tests — no external services, no fixtures.

## Project structure

| Path | Role |
|------|------|
| `cmd/pload/main.go` | Entrypoint — parses flags, creates `Downloader`, starts download |
| `internal/pload/config.go` | `Config` struct, `ParseFlags()`, constants, validation |
| `internal/pload/downloader.go` | `Downloader` struct, `NewDownloader`, `Download` orchestration, worker calc, info display |
| `internal/pload/http.go` | HTTP requests: HEAD/GET fallback, part download, `Content-Disposition` parsing |
| `internal/pload/download.go` | Single & multi-threaded download, per-part retry with backoff |
| `internal/pload/file.go` | File creation, disk space check, `formatBytes`, `basenameFromURL` |
| `internal/pload/state.go` | `PartInfo`, save/load/remove `.parts` state file |
| `internal/pload/progress.go` | Thread-safe progress bar, speed/ETA tracker, spinner |
| `internal/pload/logging.go` | Color-aware log output (`--color auto/always/never`) |
| `internal/pload/signal.go` | SIGINT/SIGTERM handler — saves state, cancels context |
| `internal/pload/disk_unix.go` | `//go:build unix` — `syscall.Statfs` disk check |
| `internal/pload/disk_windows.go` | `//go:build windows` — WinAPI disk check |
| `internal/pload/disk_other.go` | `//go:build !unix && !windows` — stub (returns error) |
| `internal/pload/pload_test.go` | Unit tests |

## Platform-specific files

Build tags matter. Changes to disk space logic should be mirrored in **three** files:
- `internal/pload/disk_unix.go` (unix)
- `internal/pload/disk_windows.go` (windows)
- `internal/pload/disk_other.go` (!unix && !windows)

## Architecture notes

- Language: English (recently translated from Russian — do not revert)
- All strings in CLI, logs, and errors are English
- `Config.Validate()` is called inside `NewDownloader()` before any work
- `Download()` orchestrates: HEAD info → part init → file alloc → download → size verification
- State file (`<filename>.parts`) is JSON, persisted per completed part
- Binary outputs to `bin/`, which is gitignored

## Testing quirks

- Test file `internal/pload/pload_test.go` has no build tags — runs everywhere
- `-count=1` in test command disables Go test cache for fresh runs
- `-race` is important — project uses `sync/atomic` and `sync.Mutex` across goroutines
