package logrotate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrClosed is returned by Write, Sync, Rotate and Reopen after shutdown starts.
var ErrClosed = errors.New("logrotate: writer is closed")

// Writer rotates one active log file while keeping its path stable.
// Compression and retention run on a background goroutine. Writer is safe for
// concurrent goroutines but assumes one process owns the file.
type Writer struct {
	cfg      config
	filename string
	dir      string
	prefix   string // file name without extension, plus "-"
	ext      string
	zipExts  []string // recognised compression suffixes, e.g. [".gz"]

	mu         sync.Mutex
	file       *os.File
	size       int64
	nextRotate time.Time // earliest upcoming time boundary; zero when disabled
	closed     bool

	millCh     chan struct{}
	millCtx    context.Context
	millCancel context.CancelCauseFunc
	millDone   chan struct{}
	errors     *errorState

	shutdownOnce sync.Once
	shutdownErr  error
}

var _ io.WriteCloser = (*Writer)(nil)

// New opens filename for append and creates missing parent directories.
// It validates all options and rotates an existing file immediately when its
// modification time belongs to an expired schedule period.
func New(filename string, opts ...Option) (*Writer, error) {
	if filename == "" {
		return nil, errors.New("logrotate: filename is required")
	}
	cfg := defaultConfig()
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("logrotate: nil option")
		}
		opt(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	filename = filepath.Clean(filename)
	base := filepath.Base(filename)
	if base == "." || base == string(filepath.Separator) {
		return nil, fmt.Errorf("logrotate: invalid filename %q", filename)
	}
	ext := filepath.Ext(base)
	millCtx, millCancel := context.WithCancelCause(context.Background())
	w := &Writer{
		cfg:        cfg,
		filename:   filename,
		dir:        filepath.Dir(filename),
		prefix:     base[:len(base)-len(ext)] + "-",
		ext:        ext,
		zipExts:    []string{".gz"},
		millCh:     make(chan struct{}, 1),
		millCtx:    millCtx,
		millCancel: millCancel,
		millDone:   make(chan struct{}),
		errors:     newErrorState(cfg.errorHandler),
	}
	if cfg.compressor != nil {
		if e := cfg.compressor.Extension(); e != ".gz" {
			w.zipExts = append(w.zipExts, e)
		}
	}

	w.mu.Lock()
	err := w.openExistingOrNewLocked()
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}

	w.errors.start()
	go w.millLoop()
	w.requestMill() // clean up leftovers from previous runs
	return w, nil
}

// Write rotates when due, then writes p in full. Maintenance stays asynchronous.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	if w.file == nil {
		// Recover from an earlier failed rotation or reopen.
		if err := w.openExistingOrNewLocked(); err != nil {
			return 0, err
		}
	}

	// The clock is only consulted when a time schedule is active or a
	// rotation actually fires; without time-based rotation the hot path
	// costs no time.Now call.
	if !w.nextRotate.IsZero() && !w.cfg.clock.Now().Before(w.nextRotate) {
		// Stamp the backup with the boundary that ended its period, not the
		// time of this write.
		if err := w.rotateLocked(w.nextRotate); err != nil {
			return 0, err
		}
	}
	if w.cfg.maxSize > 0 && w.size > 0 && w.size+int64(len(p)) > w.cfg.maxSize {
		if err := w.rotateLocked(w.cfg.clock.Now()); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)

	if err == nil && w.cfg.maxSize > 0 && w.size >= w.cfg.maxSize {
		// The file is full (or an oversized record blew past the limit);
		// rotate eagerly so it never lingers over the cap. The write itself
		// succeeded, so a rotation failure must not be returned as a write
		// failure — report it and retry on the next write.
		if rerr := w.rotateLocked(w.cfg.clock.Now()); rerr != nil {
			w.reportError(rerr)
		}
	}
	return n, err
}

// Close idempotently waits for maintenance without a deadline and returns any
// retained maintenance errors.
func (w *Writer) Close() error {
	return w.Shutdown(context.Background())
}

// Shutdown closes the active file and waits for background maintenance. Its
// result joins the active-file close error with a bounded record of maintenance
// errors. When ctx expires, in-flight compression is cancelled and the result
// also includes the context cause. A later call may keep waiting.
func (w *Writer) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("logrotate: nil shutdown context")
	}
	w.shutdownOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.shutdownErr = w.closeFileLocked()
		w.mu.Unlock()
		close(w.millCh)
	})
	select {
	case <-w.millDone:
		return w.finishShutdown()
	default:
	}

	stopCancel := context.AfterFunc(ctx, func() {
		w.millCancel(context.Cause(ctx))
	})
	defer stopCancel()
	select {
	case <-w.millDone:
		return w.finishShutdown()
	case <-ctx.Done():
		select {
		case <-w.millDone:
			return w.finishShutdown()
		default:
		}
		w.millCancel(context.Cause(ctx))
		return errors.Join(context.Cause(ctx), w.errors.err())
	}
}

// Sync flushes the active file to stable storage, satisfying
// zapcore.WriteSyncer.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if w.file == nil {
		return nil
	}
	return w.file.Sync()
}

// Rotate rotates the log immediately regardless of size or schedule, for
// example in response to SIGHUP. The backup is stamped with the current time.
// Rotating an empty file produces no backup; the file is simply reused.
func (w *Writer) Rotate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	return w.rotateLocked(w.cfg.clock.Now())
}

// Reopen closes and reopens the active file path without renaming anything.
// It is meant for coordination with external tools that move or truncate the
// log themselves (e.g. logrotate(8)); after they signal the process, Reopen
// resumes logging into a fresh file at the original path.
func (w *Writer) Reopen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if err := w.closeFileLocked(); err != nil {
		return err
	}
	return w.openExistingOrNewLocked()
}

// Filename returns the cleaned path of the active log file.
func (w *Writer) Filename() string { return w.filename }

func (w *Writer) finishShutdown() error {
	w.errors.close()
	return errors.Join(w.shutdownErr, w.errors.err())
}

// openExistingOrNewLocked prevents records from entering an expired period.
func (w *Writer) openExistingOrNewLocked() error {
	info, err := os.Stat(w.filename)
	if os.IsNotExist(err) {
		return w.openNewLocked(nil)
	}
	if err != nil {
		return fmt.Errorf("logrotate: stat log file: %w", err)
	}
	if !info.Mode().IsRegular() {
		// Refuse to touch directories, devices and the like: renaming or
		// replacing them would turn a configuration mistake into destructive
		// filesystem mutation.
		return fmt.Errorf("logrotate: %s is not a regular file (mode %v)", w.filename, info.Mode())
	}

	if boundary := w.cfg.nextRotation(info.ModTime()); !boundary.IsZero() && !w.cfg.clock.Now().Before(boundary) {
		return w.rotateLocked(boundary)
	}

	file, err := os.OpenFile(w.filename, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		// Appending is impossible (permissions, corruption); move the file
		// aside and start fresh rather than failing forever.
		return w.rotateLocked(w.cfg.clock.Now())
	}
	w.file = file
	w.size = info.Size()
	w.nextRotate = w.cfg.nextRotation(w.cfg.clock.Now())
	return nil
}

// openNewLocked preserves the previous file's mode and, on Linux, owner.
func (w *Writer) openNewLocked(prev os.FileInfo) error {
	if err := os.MkdirAll(w.dir, dirMode); err != nil {
		return fmt.Errorf("logrotate: create log directory: %w", err)
	}
	mode := w.cfg.fileMode
	if mode == 0 {
		mode = defaultFileMode
		if prev != nil {
			mode = prev.Mode().Perm()
		}
	}
	file, err := os.OpenFile(w.filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, mode)
	if err != nil {
		return fmt.Errorf("logrotate: create log file: %w", err)
	}
	// O_CREATE modes are masked by the umask; restore the exact bits.
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return fmt.Errorf("logrotate: set log file mode: %w", err)
	}
	if prev != nil {
		if err := chown(w.filename, prev); err != nil {
			w.reportError(fmt.Errorf("logrotate: preserve log file owner: %w", err))
		}
	}

	w.file = file
	w.size = 0
	// The path was free at rotation time, but another writer may have raced
	// us to it; appending keeps that data intact, so account for it.
	if info, err := file.Stat(); err == nil {
		w.size = info.Size()
	}
	w.nextRotate = w.cfg.nextRotation(w.cfg.clock.Now())
	return nil
}

// rotateLocked closes the active file, renames it to a backup stamped with
// stamp, reopens the original path and signals background maintenance. An
// empty file is reused in place rather than preserved as an empty backup.
//
// Failure modes converge instead of wedging: if the rename fails the file
// stays in place and the next write retries; if reopening fails the backup is
// already safe and the next write recreates the file.
func (w *Writer) rotateLocked(stamp time.Time) error {
	if err := w.closeFileLocked(); err != nil {
		return err
	}
	var prev os.FileInfo
	rotated := false
	info, err := os.Stat(w.filename)
	switch {
	case err == nil && !info.Mode().IsRegular():
		return fmt.Errorf("logrotate: %s is not a regular file (mode %v)", w.filename, info.Mode())
	case err == nil && info.Size() > 0:
		prev = info
		if err := os.Rename(w.filename, w.backupName(stamp)); err != nil {
			return fmt.Errorf("logrotate: rename log file: %w", err)
		}
		rotated = true
	case err == nil:
		prev = info // empty: keep it, but preserve its mode and owner
	case !os.IsNotExist(err):
		return fmt.Errorf("logrotate: stat log file: %w", err)
	}
	if err := w.openNewLocked(prev); err != nil {
		return err
	}
	if rotated {
		w.requestMill()
	}
	return nil
}

func (w *Writer) closeFileLocked() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	if err != nil {
		return fmt.Errorf("logrotate: close log file: %w", err)
	}
	return nil
}
