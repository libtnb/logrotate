package logrotate_test

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/libtnb/logrotate"
)

// The zero-configuration setup: rotate at 100 MB, keep every backup.
func Example() {
	w, err := logrotate.New(filepath.Join(os.TempDir(), "example", "app.log"))
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	log.SetOutput(w)
	log.Println("application started")
}

// A production setup: daily rotation at midnight plus a size cap, two weeks
// of compressed backups bounded to 1 GB of disk.
func Example_production() {
	w, err := logrotate.New("/var/log/myapp/app.log",
		logrotate.WithMaxSize(64*logrotate.MB),
		logrotate.WithRotateEvery(24*time.Hour),
		logrotate.WithMaxBackups(14),
		logrotate.WithMaxAge(14*logrotate.Day),
		logrotate.WithMaxTotalSize(1*logrotate.GB),
		logrotate.WithCompress(),
		logrotate.WithErrorHandler(func(err error) {
			slog.Warn("log maintenance", "err", err)
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	logger := slog.New(slog.NewJSONHandler(w, nil))
	logger.Info("application started")
}

// Rotate on SIGHUP, the conventional signal for reopening logs.
func ExampleWriter_Rotate() {
	w, err := logrotate.New(filepath.Join(os.TempDir(), "example", "app.log"))
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	log.SetOutput(w)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := w.Rotate(); err != nil {
				log.Printf("rotate: %v", err)
			}
		}
	}()
}

// Trade compression speed for ratio with a tuned built-in compressor. Any
// algorithm plugs in the same way; see the README for a zstd adapter.
func ExampleWithCompressor() {
	w, err := logrotate.New(filepath.Join(os.TempDir(), "example", "app.log"),
		logrotate.WithCompressor(logrotate.GzipCompressor{Level: 9}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = w.Close() }()
}

// fixedClock pins the wall time so that backup names are predictable. An
// immutable value is trivially safe for concurrent use, as Clock requires.
type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

// Pin the clock in tests: backup names then follow from the configuration
// alone, and rotations that share a timestamp are told apart by a sequence
// suffix.
func ExampleWithClock() {
	dir, err := os.MkdirTemp("", "logrotate-example")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	clock := fixedClock(time.Date(2026, 3, 14, 10, 30, 0, 0, time.UTC))
	w, err := logrotate.New(filepath.Join(dir, "app.log"), logrotate.WithClock(clock))
	if err != nil {
		log.Fatal(err)
	}
	for _, line := range []string{"first\n", "second\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			log.Fatal(err)
		}
		if err := w.Rotate(); err != nil {
			log.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		log.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range entries {
		fmt.Println(entry.Name())
	}
	// Output:
	// app-2026-03-14T10-30-00.000.1.log
	// app-2026-03-14T10-30-00.000.log
	// app.log
}

// Bound the wait for background compression at shutdown. A deadline that
// passes cancels the compression in flight and is joined into the result; a
// later Close waits for the maintenance goroutine to exit.
func ExampleWriter_Shutdown() {
	dir, err := os.MkdirTemp("", "logrotate-example")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	clock := fixedClock(time.Date(2026, 3, 14, 10, 30, 0, 0, time.UTC))
	w, err := logrotate.New(filepath.Join(dir, "app.log"),
		logrotate.WithClock(clock),
		logrotate.WithCompress(),
	)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := w.Write([]byte("rotated and compressed\n")); err != nil {
		log.Fatal(err)
	}
	if err := w.Rotate(); err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Fatal(err)
	}
	for _, entry := range entries {
		fmt.Println(entry.Name())
	}
	// Output:
	// app-2026-03-14T10-30-00.000.log.gz
	// app.log
}
