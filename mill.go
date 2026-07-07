package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// requestMill schedules a background maintenance pass. Signals coalesce: one
// pending pass covers any number of rotations. Callers hold w.mu, which
// orders every send strictly before the close(w.millCh) in Close.
func (w *Writer) requestMill() {
	select {
	case w.millCh <- struct{}{}:
	default:
	}
}

func (w *Writer) millLoop() {
	defer w.millWG.Done()
	for range w.millCh {
		w.millOnce()
	}
}

// millOnce performs one maintenance pass: it removes orphaned temporary
// files, enforces the backup count and age limits, compresses what remains,
// and finally enforces the total-size cap using post-compression sizes.
// Failures are reported to the error handler and skip only the affected file,
// so one bad backup cannot stall maintenance forever.
func (w *Writer) millOnce() {
	defer func() {
		if v := recover(); v != nil {
			w.cfg.reportError(fmt.Errorf("logrotate: maintenance panic: %v", v))
		}
	}()

	backups, orphans, err := w.listBackups()
	if err != nil {
		w.cfg.reportError(err)
		return
	}
	for _, name := range orphans {
		w.removeFile(name)
	}

	keep := backups[:0]
	cutoff := w.cfg.clock.Now().Add(-w.cfg.maxAge)
	for i, b := range backups {
		switch {
		case w.cfg.maxBackups > 0 && i >= w.cfg.maxBackups,
			w.cfg.maxAge > 0 && b.stamp.Before(cutoff):
			w.removeBackup(b)
		default:
			keep = append(keep, b)
		}
	}
	backups = keep

	if w.cfg.compressor != nil {
		ext := w.cfg.compressor.Extension()
		for _, b := range backups {
			if b.compressed() {
				// Finish an interrupted pass: drop a plain file whose archive
				// already exists.
				b.files = slices.DeleteFunc(b.files, func(f backupFile) bool {
					if f.compressed {
						return false
					}
					w.removeFile(f.name)
					return true
				})
				continue
			}
			src := filepath.Join(w.dir, b.files[0].name)
			size, err := w.compressFile(src, src+ext)
			if err != nil {
				w.cfg.reportError(err)
				continue
			}
			b.files = []backupFile{{name: b.files[0].name + ext, size: size, compressed: true}}
		}
	}

	if w.cfg.maxTotalSize > 0 {
		var total int64
		for _, b := range backups {
			total += b.size()
			if total > w.cfg.maxTotalSize {
				w.removeBackup(b)
			}
		}
	}
}

func (w *Writer) removeBackup(b *backup) {
	for _, f := range b.files {
		w.removeFile(f.name)
	}
}

func (w *Writer) removeFile(name string) {
	if err := os.Remove(filepath.Join(w.dir, name)); err != nil && !os.IsNotExist(err) {
		w.cfg.reportError(fmt.Errorf("logrotate: remove old backup: %w", err))
	}
}
