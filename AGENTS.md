# AGENTS.md

## Build & Run

```bash
go build -o bin/pload        # build binary
go vet ./...                 # lint (no golangci-lint configured)
```

No test files exist. `go test ./...` runs but finds nothing.

## Project Structure

Single Go package (`package main`), no subpackages. All logic in:
- `main.go` — downloader core, CLI parsing, progress bar
- `disk_unix.go` / `disk_windows.go` / `disk_other.go` — disk space checks via build tags (`unix`, `windows`, `!unix && !windows`)

Module: `github.com/i-panov/pload`. Go 1.24.2 (go.mod). Two direct deps: `schollz/progressbar/v3`, `spf13/pflag`.

## Conventions

- **Language**: Code comments, log messages, CLI help, and README are all in Russian. Keep consistent.
- **Build tags**: Platform-specific disk code uses `//go:build` directives. Don't merge platform files.
- **No test framework**: If adding tests, keep it simple — `testing` package only, no external test deps unless the user asks.
- **Flags**: Uses `spf13/pflag` (POSIX-style), not `flag` stdlib. Short flags: `-u`, `-o`, `-w`, `-t`, `-a`, `-f`, `-v`, `-k`.
