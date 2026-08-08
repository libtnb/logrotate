# logrotate

[![Go Reference](https://pkg.go.dev/badge/github.com/libtnb/logrotate.svg)](https://pkg.go.dev/github.com/libtnb/logrotate)
[![Test](https://github.com/libtnb/logrotate/actions/workflows/test.yml/badge.svg)](https://github.com/libtnb/logrotate/actions)

A concurrent `io.WriteCloser` for size- and time-based log rotation. The active
path stays stable; compression and retention run on one maintenance goroutine.
Requires Go 1.27.

```sh
go get github.com/libtnb/logrotate
```

## Usage

```go
w, err := logrotate.New(
	"/var/log/myapp/app.log",
	logrotate.WithMaxSize(64*logrotate.MB),
	logrotate.WithRotateEvery(24*time.Hour),
	logrotate.WithMaxBackups(14),
	logrotate.WithMaxAge(14*logrotate.Day),
	logrotate.WithCompress(),
)
if err != nil {
	log.Fatal(err)
}
defer func() {
	if err := w.Close(); err != nil {
		log.Printf("log maintenance: %v", err)
	}
}()

log.SetOutput(w)
```

`New` validates the complete configuration, creates the parent directory, and
opens the file before returning.

| Option | Effect |
| --- | --- |
| `WithMaxSize` | Size limit; default 100 MB, zero disables |
| `WithRotateEvery` / `WithRotateAt` | Wall-clock rotation schedule |
| `WithMaxBackups` | Backup count limit |
| `WithMaxAge` | Backup age limit |
| `WithMaxTotalSize` | Total compressed/on-disk backup limit |
| `WithCompress` / `WithCompressor` | Background compression |
| `WithLocation` | Schedule and backup-name timezone; default UTC |
| `WithErrorHandler` | Prompt advisory maintenance notifications |

Time boundaries are checked on writes, so idle periods create no empty backup.
Oversized records are written in full and rotated afterward. Timestamp
collisions use a sequence suffix; an existing backup is never overwritten.
Files outside this writer's backup naming pattern are never removed.

## Shutdown and errors

`Close` waits for maintenance and returns a bounded aggregate of retained
compression/cleanup errors. `Shutdown` adds a deadline:

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
if err := w.Shutdown(ctx); err != nil {
	log.Printf("log shutdown: %v", err)
}
```

The shutdown context reaches the active `Compressor`; custom implementations
should honor cancellation. `WithErrorHandler` is notification-only: handlers
run serially outside Writer locks, and a slow handler cannot block writes or
maintenance. `Close` and `Shutdown` remain the authoritative error result.

After shutdown begins, `Write`, `Sync`, `Rotate`, and `Reopen` return
`ErrClosed`. `Reopen` is intended for an external rotator that moved or
truncated the active file.

`Writer` is safe for concurrent goroutines but assumes one process owns the
active path. It works directly with `log`, `slog`, and zap-style `Sync` users.

API details and signal examples are on
[pkg.go.dev](https://pkg.go.dev/github.com/libtnb/logrotate).
