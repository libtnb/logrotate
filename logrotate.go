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

// ErrClosed is returned, unwrapped, by Write, Sync, Rotate and Reopen once
// Close or Shutdown has been called. A closed Writer cannot be reopened;
// create a new one with New.
var ErrClosed = errors.New("logrotate: writer is closed")

// Writer is an io.WriteCloser that appends to one log file and moves it aside
// as a timestamped backup when a size or time limit is reached; the package
// documentation describes the rotation, naming and retention rules.
// Compression and retention run on a maintenance goroutine that Close or
// Shutdown stops. Create a Writer with New; the zero value is not usable.
//
// A Writer is safe for concurrent use by multiple goroutines. It assumes it is
// the only process owning the active path: it takes no file lock, so two
// processes rotating the same path can overwrite each other's backups.
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
	lastStamp  string // formatted timestamp of the latest backup name issued
	lastSeq    int    // its sequence; the next rotation on the same stamp continues from here

	millCh     chan struct{}
	millCtx    context.Context
	millCancel context.CancelCauseFunc
	millDone   chan struct{}
	errors     *errorState

	shutdownOnce sync.Once
	shutdownErr  error
}

var _ io.WriteCloser = (*Writer)(nil)

// New opens filename for append, creating missing parent directories, and
// starts the maintenance goroutine. The options are applied in order and the
// complete set is validated before the file system is touched.
//
// An existing file is rotated immediately when its modification time belongs
// to a rotation period that has already ended, so records written after a
// restart never land in an expired period, and when it cannot be opened for
// append, so a permission problem moves the old file aside instead of failing
// every write. The first maintenance pass starts right away and finishes
// compression or cleanup left over from a previous run.
//
// New returns an error when filename is empty or has no base name, when an
// option is nil, when the options fail validation (a negative limit, a
// WithRotateEvery outside [1s, 24h], a malformed WithRotateAt time, a
// WithBackupTimeFormat layout that does not round-trip or contains a path
// separator, a WithFileMode with non-permission bits, a nil Clock or location,
// a typed-nil Compressor, or a Compressor with an invalid extension), when
// filename exists but is not a regular file, or when the directory or file
// cannot be created, opened or rotated. Validation errors are descriptive but
// not sentinel values.
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
		// ".gz" stays recognised so that archives from an earlier
		// WithCompress configuration remain under retention.
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

// Write appends p to the active file as one unit. A rotation that is due, be
// it a time boundary that has passed or a size limit that p would push the
// file past, happens before the write, so a record is never split across
// files; a record larger than the size limit is written in full and rotated
// out right after it. Rotation only renames and reopens; compression and
// retention are left to the maintenance goroutine. If an earlier failure left
// no file open, Write reopens the path first.
//
// Write returns ErrClosed after Close or Shutdown. If a rotation that must
// precede the write fails, nothing is written and the error is returned; the
// next Write retries. If the rotation after a file-filling or oversized
// record fails, the write has already succeeded, so the error is reported to
// the error handler and retained for Close instead of being returned.
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
	sizeLimited := w.cfg.maxSize > 0
	wouldOverflow := w.size > 0 && w.size+int64(len(p)) > w.cfg.maxSize
	if sizeLimited && wouldOverflow {
		if err := w.rotateLocked(w.cfg.clock.Now()); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)

	full := sizeLimited && w.size >= w.cfg.maxSize
	if err == nil && full {
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

// Close stops the Writer: it closes the active file, waits without a deadline
// for maintenance to finish and returns the close error joined with the
// retained maintenance errors described at Shutdown; it is
// Shutdown(context.Background()). Close is idempotent and safe to call
// concurrently, and later calls report the same retained errors.
func (w *Writer) Close() error {
	return w.Shutdown(context.Background())
}

// Shutdown stops the Writer like Close, bounded by ctx. It closes the active
// file, after which Write, Sync, Rotate and Reopen return ErrClosed, then
// waits for the maintenance goroutine to finish its pending pass. When ctx
// ends first, the in-flight compression is cancelled with the context cause,
// Shutdown returns at once with the cause joined into its result, and a later
// Shutdown or Close keeps waiting for maintenance to exit.
//
// The result joins the file close error with a bounded record of maintenance
// errors: the first one, the most recent ones and a count of those omitted in
// between. Maintenance errors come from compression, listing or removing
// backups, ownership preservation and rotations that failed after a
// successful write. A nil result means the file closed cleanly and no
// maintenance error was ever reported. Shutdown returns an error for a nil
// ctx. It is idempotent and safe to call concurrently, including from a
// WithErrorHandler callback.
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

// Sync flushes the active file to stable storage, which makes Writer a
// zapcore.WriteSyncer. It returns ErrClosed after Close or Shutdown and nil
// when no file is open, which is the case after a failed rotation or Reopen
// until the next Write reopens the path.
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

// Rotate moves the active file aside as a backup stamped with the current
// time and reopens the path, regardless of size or schedule; call it from a
// SIGHUP handler or another external trigger. An empty active file produces
// no backup and is kept in place. Rotate returns ErrClosed after Close or
// Shutdown. When the rename fails the file stays in place and the error is
// returned; when reopening fails the backup is already safe, the error is
// returned and the next Write recreates the file.
func (w *Writer) Rotate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	return w.rotateLocked(w.cfg.clock.Now())
}

// Reopen closes the active file and opens the path again without renaming
// anything. Use it with an external rotator such as logrotate(8): once the
// tool has moved or truncated the log and signalled the process, Reopen
// resumes logging at the original path, creating the file when the tool
// moved it away. Pair it with WithMaxSize(0) to delegate rotation entirely.
// Reopen returns ErrClosed after Close or Shutdown and the close or open
// error otherwise; after a failed open the next Write retries.
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

// Filename returns the cleaned path of the active log file passed to New.
func (w *Writer) Filename() string { return w.filename }

// finishShutdown runs once maintenance has exited: it stops error
// notification and assembles the shutdown result.
func (w *Writer) finishShutdown() error {
	w.errors.close()
	return errors.Join(w.shutdownErr, w.errors.err())
}

// openExistingOrNewLocked opens the path for append. It rotates first when the
// existing file's modification time belongs to a schedule period that has
// ended, so records never enter an expired period, and when appending is
// impossible, so the old file is moved aside instead of failing forever.
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

	boundary := w.cfg.nextRotation(info.ModTime())
	periodEnded := !boundary.IsZero() && !w.cfg.clock.Now().Before(boundary)
	if periodEnded {
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

// openNewLocked creates the active file, preserving the previous file's mode
// and, on Linux, owner unless WithFileMode dictates the mode.
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

// closeFileLocked closes the active file and clears w.file so that a later
// Write reopens the path.
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
