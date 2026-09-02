package logrotate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// The maintenance goroutine runs one pass per request; requests coalesce, so
// a burst of rotations is served by a single pass. Each pass works on a fresh
// directory listing and applies, in order:
//
//  1. removal of orphaned ".tmp" archives left by an interrupted compression,
//  2. WithMaxBackups and WithMaxAge over the backups sorted newest first,
//  3. compression of the survivors, first finishing any interrupted archive,
//  4. WithMaxTotalSize over the resulting on-disk sizes.
//
// Retention runs before compression so no work is spent on a backup about to
// be deleted, and the size cap runs after it so it counts what is actually on
// disk. Every step checks the context, which Shutdown cancels when its
// deadline passes; a failure on one file is reported and skips only that
// file.

// requestMill schedules a background maintenance pass. Signals coalesce: one
// pending pass covers any number of rotations. Callers other than New hold
// w.mu, which orders every send strictly before the close(w.millCh) in
// Shutdown.
func (w *Writer) requestMill() {
	select {
	case w.millCh <- struct{}{}:
	default:
	}
}

// millLoop runs passes until Shutdown closes w.millCh or cancels w.millCtx,
// then signals w.millDone.
func (w *Writer) millLoop() {
	defer close(w.millDone)
	for {
		select {
		case <-w.millCtx.Done():
			return
		case _, ok := <-w.millCh:
			if !ok {
				return
			}
			w.millOnce(w.millCtx)
		}
	}
}

// millOnce performs one maintenance pass in the order described at the top of
// this file. Failures are reported to the error handler and skip only the
// affected file, so one bad backup cannot stall maintenance forever.
func (w *Writer) millOnce(ctx context.Context) {
	defer func() {
		if v := recover(); v != nil {
			w.reportError(fmt.Errorf("logrotate: maintenance panic: %v", v))
		}
	}()
	if err := ctx.Err(); err != nil {
		return
	}

	backups, orphans, err := w.listBackups()
	if err != nil {
		w.reportError(err)
		return
	}
	for _, name := range orphans {
		if ctx.Err() != nil {
			return
		}
		w.removeFile(name)
	}

	keep := backups[:0]
	cutoff := w.cfg.clock.Now().Add(-w.cfg.maxAge)
	for i, b := range backups {
		if ctx.Err() != nil {
			return
		}
		overCount := w.cfg.maxBackups > 0 && i >= w.cfg.maxBackups
		expired := w.cfg.maxAge > 0 && b.stamp.Before(cutoff)
		if overCount || expired {
			w.removeBackup(b)
			continue
		}
		keep = append(keep, b)
	}
	backups = keep

	if w.cfg.compressor != nil {
		ext := w.cfg.compressor.Extension()
		for _, b := range backups {
			if ctx.Err() != nil {
				return
			}
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
			size, err := w.compressFile(ctx, src, src+ext)
			if err != nil {
				if ctx.Err() != nil && errors.Is(err, context.Cause(ctx)) {
					return
				}
				w.reportError(err)
				continue
			}
			b.files = []backupFile{{name: b.files[0].name + ext, size: size, compressed: true}}
		}
	}

	if w.cfg.maxTotalSize > 0 {
		var total int64
		for _, b := range backups {
			if ctx.Err() != nil {
				return
			}
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

// removeFile deletes one file of the log directory, treating an already
// missing file as success.
func (w *Writer) removeFile(name string) {
	if err := os.Remove(filepath.Join(w.dir, name)); err != nil && !os.IsNotExist(err) {
		w.reportError(fmt.Errorf("logrotate: remove old backup: %w", err))
	}
}
