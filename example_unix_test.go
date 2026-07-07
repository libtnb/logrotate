//go:build unix

package logrotate_test

import (
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/libtnb/logrotate"
)

// Let an external tool such as logrotate(8) move the file, then reopen the
// original path on SIGUSR1 instead of rotating in-process.
func ExampleWriter_Reopen() {
	w, err := logrotate.New(filepath.Join(os.TempDir(), "example", "app.log"),
		logrotate.WithMaxSize(0), // rotation is fully delegated to the external tool
	)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	log.SetOutput(w)

	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	go func() {
		for range usr1 {
			if err := w.Reopen(); err != nil {
				log.Printf("reopen: %v", err)
			}
		}
	}()
}
