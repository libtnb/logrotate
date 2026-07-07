package logrotate_test

import (
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
	defer w.Close()

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
	defer w.Close()

	logger := slog.New(slog.NewJSONHandler(w, nil))
	logger.Info("application started")
}

// Rotate on SIGHUP, the conventional signal for reopening logs.
func ExampleWriter_Rotate() {
	w, err := logrotate.New(filepath.Join(os.TempDir(), "example", "app.log"))
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()
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
	defer w.Close()
}
