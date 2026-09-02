# logrotate

[![Go Version](https://img.shields.io/github/go-mod/go-version/libtnb/logrotate)](https://go.dev/) [![License](https://img.shields.io/github/license/libtnb/logrotate)](./LICENSE) [![Build Status](https://img.shields.io/github/actions/workflow/status/libtnb/logrotate/test.yml?branch=main)](https://github.com/libtnb/logrotate/actions) [![Go Report Card](https://goreportcard.com/badge/github.com/libtnb/logrotate)](https://goreportcard.com/report/github.com/libtnb/logrotate) [![Go Reference](https://pkg.go.dev/badge/github.com/libtnb/logrotate.svg)](https://pkg.go.dev/github.com/libtnb/logrotate)

A rotating log file writer for Go: an `io.WriteCloser` that appends to one file at a fixed path, moves it aside as a timestamped backup on size or wall-clock boundaries, and compresses and prunes the backups on a maintenance goroutine that never blocks writes. Standard library only.

```go
w, err := logrotate.New("/var/log/myapp/app.log",
	logrotate.WithMaxSize(64*logrotate.MB),
	logrotate.WithRotateEvery(24*time.Hour),
	logrotate.WithMaxBackups(14),
	logrotate.WithCompress(),
)
if err != nil {
	log.Fatal(err)
}
defer func() {
	if err := w.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "log maintenance:", err)
	}
}()

log.SetOutput(w)
log.Println("application started")
```

## 🚀 Getting Started

Requires Go 1.27 or later.

```bash
go get github.com/libtnb/logrotate
```

```go
package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/libtnb/logrotate"
)

func main() {
	w, err := logrotate.New("/var/log/myapp/app.log",
		logrotate.WithMaxSize(64*logrotate.MB),
		logrotate.WithRotateEvery(24*time.Hour),
		logrotate.WithMaxBackups(14),
		logrotate.WithMaxAge(14*logrotate.Day),
		logrotate.WithCompress(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() {
		// The Writer is closed by the time Close returns, so report its
		// result somewhere else.
		if err := w.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "log maintenance:", err)
		}
	}()

	logger := slog.New(slog.NewJSONHandler(w, nil))
	logger.Info("application started")
}
```

`New` validates the complete option set, creates missing parent directories, opens the file for append and starts the maintenance goroutine. `Writer` is a plain `io.Writer` for `log`, `slog` and friends, and its `Sync` method makes it a `zapcore.WriteSyncer`.

## ✨ Features

### Options

| Option | Default | Accepted values |
| --- | --- | --- |
| `WithMaxSize(n int64)` | `100*MB` | `n >= 0`; `0` disables size rotation |
| `WithRotateEvery(d time.Duration)` | off | `0` (off) or `1s <= d <= 24h` |
| `WithRotateAt(times ...string)` | none | `"HH:MM"` with hour `0-23` and minute `0-59`; calls accumulate |
| `WithMaxBackups(n int)` | `0` (keep all) | `n >= 0` |
| `WithMaxAge(d time.Duration)` | `0` (keep all) | `d >= 0` |
| `WithMaxTotalSize(n int64)` | `0` (unlimited) | `n >= 0` |
| `WithCompress()` | off | gzip at `gzip.DefaultCompression` |
| `WithCompressor(c Compressor)` | `nil` (off) | not a typed nil; `Extension()` starts with `.`, has at least two characters, is not `.tmp` and contains no path separator |
| `WithLocation(loc *time.Location)` | `time.UTC` | non-nil |
| `WithBackupTimeFormat(layout string)` | `"2006-01-02T15-04-05.000"` | non-empty, no path separator, round-trips through `time.Parse` |
| `WithFileMode(mode fs.FileMode)` | inherited; `0o600` for a brand-new file | permission bits only |
| `WithErrorHandler(fn func(error))` | none | any function |
| `WithClock(c Clock)` | `time.Now` | non-nil, safe for concurrent use |

`KB`, `MB` and `GB` are binary units; `Day` is `24 * time.Hour` without calendar semantics. `New` checks the whole set before touching the file system and returns a descriptive, non-sentinel error for an invalid value, a nil option, an empty file name, or a path that exists but is not a regular file.

### Size-based rotation

With `WithMaxSize(n)` a `Write` whose record would push the file past `n` bytes rotates first, and a record that fills the file exactly is followed by an immediate rotation, so the active file never lingers over the cap. Records are never split across files; a single record larger than `n` is written in full and rotated out on its own. `WithMaxSize(0)` turns size rotation off, for example when an external tool owns rotation (see [Signals and external rotators](#signals-and-external-rotators)).

### Time-based rotation

```go
shanghai, err := time.LoadLocation("Asia/Shanghai")
if err != nil {
	log.Fatal(err)
}
w, err := logrotate.New("/var/log/myapp/app.log",
	logrotate.WithRotateEvery(6*time.Hour), // 00:00, 06:00, 12:00, 18:00
	logrotate.WithRotateAt("03:30"),        // plus a fixed daily time
	logrotate.WithLocation(shanghai),       // boundaries and names in this zone
)
```

- Boundaries are evaluated on writes, not by a timer. The first `Write` at or after a boundary rotates before its record is written, an idle period produces no empty backup, and the backup is stamped with the boundary that ended its period rather than with the time of the write.
- `WithRotateEvery` boundaries are anchored at midnight in the configured zone. The last interval of a day is cut at the next midnight, so `24*time.Hour` means calendar midnight even on a 23- or 25-hour daylight-saving day. `WithRotateAt` adds fixed times of day; with both, the earlier boundary wins.
- `New` rotates an existing file right away when its modification time falls in a period that has already ended, so records written after a restart never land in an expired period.

### Backup naming

```text
app.log                                active file
app-2026-03-14T10-30-00.000.log        backup rotated at 10:30:00 UTC
app-2026-03-14T10-30-00.000.1.log      second backup with the same timestamp
app-2026-03-14T10-30-00.000.1.log.gz   the same backup after compression
```

Backups sit next to the active file and carry the rotation time in the `WithBackupTimeFormat` layout, rendered in the `WithLocation` zone. When a timestamp is already taken, because the layout is coarser than the rotation cadence or two rotations fell into one tick, a sequence suffix is inserted before the extension. Sequences within a timestamp only ever grow, one past the highest found on disk, even after retention has removed earlier ones, so the newest backup always sorts last and no existing backup is overwritten. `WithBackupTimeFormat("2006-01-02")` therefore gives day-precision names such as `app-2026-03-14.log`, `app-2026-03-14.1.log` and so on. Files in the directory that do not match this pattern are never removed.

### Retention

Every maintenance pass, requested by `New` and by each rotation that produced a backup, works on a fresh directory listing sorted newest first by timestamp and then by sequence, and applies in this order:

1. removal of orphaned `.tmp` archives left by an interrupted compression;
2. `WithMaxBackups` and `WithMaxAge`, the latter measured from the backup timestamp against the `Clock`, not from the file modification time;
3. compression of the survivors;
4. `WithMaxTotalSize` over the resulting on-disk sizes, so archives count at their compressed size and a backup whose compression failed counts in full. The active file is not counted.

### Compression

```go
logrotate.WithCompress()                                                  // gzip, default level
logrotate.WithCompressor(logrotate.GzipCompressor{Level: gzip.BestSpeed}) // tuned gzip
```

Backups are compressed one at a time on the maintenance goroutine, after retention. An archive is staged under a `.tmp` suffix, synced and renamed into place before the plain backup is removed, so an interruption leaves the plain file, the finished archive or both, never a truncated archive under its final name; the next pass completes the job. A backup whose compression fails stays uncompressed and is retried on the next pass.

Any algorithm plugs in through the `Compressor` interface. The Writer hands `Compress` a staging file as `dst` and publishes it when `Compress` returns nil, so an implementation must return an error whenever the archive is incomplete, and should return promptly with an error wrapping `context.Cause(ctx)` once `ctx` is done, which is how a `Shutdown` deadline stops the pass. `Extension` must start with a dot and stay constant; archives ending in `.gz` are always recognised, so switching away from `WithCompress` keeps earlier archives under retention. A zstd adapter built on `github.com/klauspost/compress/zstd`:

```go
type zstdCompressor struct{}

func (zstdCompressor) Extension() string { return ".zst" }

func (zstdCompressor) Compress(ctx context.Context, dst io.Writer, src io.Reader) error {
	enc, err := zstd.NewWriter(dst)
	if err != nil {
		return err
	}
	if _, err := enc.ReadFrom(cancelReader{ctx: ctx, r: src}); err != nil {
		_ = enc.Close()
		return err
	}
	return enc.Close()
}

// cancelReader fails the next read once ctx is done, so a large archive is
// abandoned between chunks instead of running to the end.
type cancelReader struct {
	ctx context.Context
	r   io.Reader
}

func (r cancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, context.Cause(r.ctx)
	}
	return r.r.Read(p)
}
```

### Shutdown and errors

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
if err := w.Shutdown(ctx); err != nil {
	fmt.Fprintln(os.Stderr, "log shutdown:", err)
}
```

- `Close` is `Shutdown(context.Background())`. Both close the active file, after which `Write`, `Sync`, `Rotate` and `Reopen` return `ErrClosed` (unwrapped, compare with `errors.Is`), and wait for the maintenance goroutine to finish its pending pass. `Shutdown` stops waiting when its context ends, cancels the compression in flight and joins the context cause into its result; a later `Close` or `Shutdown` waits for the goroutine to exit.
- The result joins the file close error with a bounded record of the maintenance errors reported during the Writer's life: the first one, the most recent ones and the number omitted in between. Maintenance errors come from compression, listing or removing backups, ownership preservation, and rotations that failed after a successful write. Both calls are idempotent and safe to call concurrently, and later calls report the same record.
- `WithErrorHandler` receives the same errors as they happen, serially on a dedicated goroutine that never holds a Writer lock, so a slow handler cannot delay writes or maintenance; a panic in the handler is contained. Delivery is advisory: when notifications arrive faster than the handler drains them, the excess is dropped and summarised in one `error handler notifications omitted` error, and `Close` may return before the last notifications have been delivered. `Close` and `Shutdown` remain the authoritative record. The handler may call `Shutdown` itself.

### Signals and external rotators

`Rotate` moves the active file aside regardless of size or schedule, the conventional response to `SIGHUP`; an empty active file produces no backup. `Reopen` closes and reopens the path without renaming anything, for deployments where a tool such as logrotate(8) moves or truncates the file and then signals the process; combine it with `WithMaxSize(0)` to delegate rotation entirely. Both return `ErrClosed` after shutdown.

```go
hup := make(chan os.Signal, 1)
signal.Notify(hup, syscall.SIGHUP)
go func() {
	for range hup {
		if err := w.Rotate(); err != nil {
			fmt.Fprintln(os.Stderr, "rotate:", err)
		}
	}
}()
```

`Sync` flushes the active file to stable storage, returns `ErrClosed` after shutdown and nil when no file is currently open.

### Deterministic tests with Clock

`WithClock` replaces the wall-clock source used for schedules, backup names and age-based retention, so tests can drive rotation without sleeping. The Writer reads the clock under its write lock and, concurrently, from the maintenance goroutine, so an implementation must be safe for concurrent use; an immutable value is.

```go
type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

w, err := logrotate.New(filepath.Join(t.TempDir(), "app.log"),
	logrotate.WithClock(fixedClock(time.Date(2026, 3, 14, 10, 30, 0, 0, time.UTC))),
	logrotate.WithRotateEvery(time.Hour),
)
```

`ExampleWithClock` on [pkg.go.dev](https://pkg.go.dev/github.com/libtnb/logrotate#example-WithClock) shows the resulting backup names.

## 🤝 Contributing

Please read the [contributing guide](CONTRIBUTING.md) before submitting a PR.

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
