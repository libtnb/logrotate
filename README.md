# logrotate

[![Doc](https://pkg.go.dev/badge/github.com/libtnb/logrotate)](https://pkg.go.dev/github.com/libtnb/logrotate)
[![Go](https://img.shields.io/github/go-mod/go-version/libtnb/logrotate)](https://go.dev/)
[![Release](https://img.shields.io/github/release/libtnb/logrotate.svg)](https://github.com/libtnb/logrotate/releases)
[![Test](https://github.com/libtnb/logrotate/actions/workflows/test.yml/badge.svg)](https://github.com/libtnb/logrotate/actions)
[![Report Card](https://goreportcard.com/badge/github.com/libtnb/logrotate)](https://goreportcard.com/report/github.com/libtnb/logrotate)
[![Stars](https://img.shields.io/github/stars/libtnb/logrotate?style=flat)](https://github.com/libtnb/logrotate)
[![License](https://img.shields.io/github/license/libtnb/logrotate)](https://opensource.org/license/MIT)

A modern rotating-file writer for Go logs, with no third-party dependencies.

`logrotate.Writer` is an `io.WriteCloser` that sits at the bottom of any
logging stack — `log`, `slog`, `zap`, `zerolog`, anything that writes to an
`io.Writer` — and takes care of everything that happens to the file
afterwards: size- and clock-based rotation, backup naming, gzip compression,
and retention by count, age and total disk usage.

## Features

- **Validated configuration.** `New` returns an error for a bad option — an
  unwritable path, an invalid schedule, a broken timestamp layout — instead of
  degrading silently at the first write. Options are immutable afterwards, so
  there is no configuration race by design.
- **Byte-precision size limit** (`WithMaxSize(64*logrotate.MB)`) with
  readable size constants. A record larger than the cap is still written in
  full and isolated into its own backup; log data is never rejected.
- **Wall-clock rotation without a timer goroutine.** `WithRotateEvery(d)`
  rotates on boundaries anchored at midnight (hourly on the hour, daily at
  midnight); `WithRotateAt("03:00")` rotates at fixed times of day. Boundaries
  are evaluated on write: an idle service creates no empty files, and backups
  are stamped with the boundary that ended their period, not the arbitrary
  moment the next write happened.
- **Restart-aware.** The current rotation period is recovered from the log
  file's modification time, so a leftover file from yesterday is rotated out
  at startup instead of collecting today's records.
- **Three retention dimensions**: `WithMaxBackups` (count), `WithMaxAge`
  (a `time.Duration`, not "days"), and `WithMaxTotalSize` — a hard disk quota
  for all backups combined, enforced on post-compression sizes.
- **Crash-safe compression.** Backups are compressed in the background to a
  temporary file, fsynced, then renamed into place; an interrupted pass is
  detected and finished on the next run. gzip is built in; any algorithm
  (zstd, lz4, …) plugs in through the two-method `Compressor` interface
  without adding dependencies to this package.
- **Collision-proof backup names.** Rotating twice within one timestamp
  granule appends a sequence (`app-2026-07-07.1.log`), so backups are never
  overwritten. Empty files are never rotated into empty backups.
- **Observable background work.** Compression and cleanup errors go to your
  `WithErrorHandler` callback instead of being lost.
- **Clean lifecycle.** `Close` is idempotent, stops the maintenance
  goroutine, and waits for pending compression to finish; writes after close
  fail fast with `ErrClosed`. `Sync` satisfies zap's `WriteSyncer`; `Reopen`
  cooperates with external tools like logrotate(8).
- **Fast.** One mutex, no allocations on the write path (0 B/op), ~50 ns
  added by schedule checks.

## Installation

```bash
go get github.com/libtnb/logrotate
```

## Quick start

```go
package main

import (
	"log"
	"time"

	"github.com/libtnb/logrotate"
)

func main() {
	w, err := logrotate.New("/var/log/myapp/app.log",
		logrotate.WithMaxSize(64*logrotate.MB),     // rotate past 64 MB…
		logrotate.WithRotateEvery(24*time.Hour),    // …or at midnight, whichever first
		logrotate.WithMaxBackups(14),               // keep at most 14 backups
		logrotate.WithMaxAge(14*logrotate.Day),     // and none older than two weeks
		logrotate.WithMaxTotalSize(1*logrotate.GB), // bounded to 1 GB of disk
		logrotate.WithCompress(),                   // gzipped in the background
	)
	if err != nil {
		log.Fatal(err) // bad config or unwritable path surfaces here
	}
	defer w.Close()

	log.SetOutput(w)
	log.Println("ready")
}
```

The file passed to `New` is always the active log file. Rotation renames it to
`app-2026-07-07T00-00-00.000.log` (then `.log.gz` once compressed) in the same
directory and reopens the original path, so `tail -F` and shippers keep
working. Every default is safe: 100 MB size cap, no time rotation, keep
everything, no compression, UTC timestamps, `0o600` files.

## Rotation triggers

| Trigger | Option | Backup timestamp |
|---|---|---|
| File reaches the size cap | `WithMaxSize(n)` | time of rotation |
| Wall-clock boundary passes | `WithRotateEvery(d)` | the boundary itself |
| Time of day passes | `WithRotateAt("HH:MM", ...)` | the boundary itself |
| Manual | `w.Rotate()` | time of rotation |

Time-based triggers fire on the first write after the boundary. That write is
placed in the *new* file; everything before it is rotated out under the
boundary's timestamp. If several boundaries pass while the process is idle or
down, only one rotation happens — no empty catch-up files.

`WithRotateEvery` accepts 1s to 24h and anchors at midnight in the configured
time zone (UTC by default, any zone with `WithLocation`), so every day repeats
the same boundary sequence regardless of when the process started.

## Retention and compression

After every rotation a single background goroutine:

1. removes orphaned temporary files from an interrupted compression,
2. deletes backups beyond `WithMaxBackups` or older than `WithMaxAge`,
3. compresses the survivors (if a compressor is configured),
4. deletes the oldest backups until the rest fit in `WithMaxTotalSize`.

Failures affect only the file involved, are reported to `WithErrorHandler`,
and are retried on the next pass. The total-size quota always reflects actual
bytes on disk — a backup whose compression failed counts at its uncompressed
size, so the disk bound holds even when compression cannot make progress.
Files in the directory that don't match this writer's backup pattern are
never touched.

To plug in another algorithm, implement two methods:

```go
type ZstdCompressor struct{}

func (ZstdCompressor) Compress(dst io.Writer, src io.Reader) error {
	enc, err := zstd.NewWriter(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(enc, src); err != nil {
		enc.Close()
		return err
	}
	return enc.Close()
}

func (ZstdCompressor) Extension() string { return ".zst" }

// logrotate.New(path, logrotate.WithCompressor(ZstdCompressor{}))
```

## Integrations

**slog**

```go
logger := slog.New(slog.NewJSONHandler(w, nil))
```

**zap** — `*Writer` implements `zapcore.WriteSyncer` directly:

```go
core := zapcore.NewCore(zapcore.NewJSONEncoder(cfg), w, zap.InfoLevel)
```

**Very high volume** — the writer deliberately performs one `write` syscall
per record, so a record handed to a successful `Write` is in the kernel, not
in a user-space buffer that a crash would lose. If you log at a rate where
syscalls dominate, add your stack's buffering layer on top — it composes
cleanly and keeps the durability trade-off in your hands:

```go
ws := &zapcore.BufferedWriteSyncer{WS: w, Size: 256 * 1024} // zap
// or bufio.NewWriter(w) with a periodic Flush for other stacks
```

**SIGHUP** (rotate in-process) and **logrotate(8)** (rotate externally,
`Reopen` on signal) patterns are shown in the
[package examples](https://pkg.go.dev/github.com/libtnb/logrotate#pkg-examples).

## Design notes

- **Rotation is evaluated on write, never by a timer.** A background timer
  has to coordinate with writers, wakes idle processes, and rotates files
  nobody is writing to. Checking the boundary at write time costs one time
  comparison, produces identical files, and makes the whole schedule
  trivially testable.
- **Backups are data, not metadata.** The file name carries exactly one fact
  — when its period ended (plus a collision sequence). Rotation *reasons*,
  hostnames and the like belong in the log records themselves; encoding them
  in file names makes cleanup parsing fragile.
- **Everything the writer does in the background is bounded and observable.**
  One maintenance goroutine, woken only by rotations, whose failures reach
  your error handler and whose shutdown is drained by `Close`.
- **One process owns a given log file.** Cooperative multi-process rotation
  needs file locks and is out of scope; use `Reopen` with an external rotator
  if another process must drive rotation.

## License

MIT
