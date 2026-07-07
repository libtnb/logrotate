package logrotate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrClosed is returned by Write, Rotate and Reopen after Close.
var ErrClosed = errors.New("logrotate: writer is closed")

// Writer is a rotating file writer implementing io.WriteCloser.
//
// The file given to New is always the active log file; rotation renames it to
// a backup in the same directory ("name-<timestamp>.ext", plus a ".<n>"
// sequence on collision and the compressor extension once compressed) and
// reopens the original path, so external tools can rely on a stable name.
// Rotation happens when the file reaches its size limit, when a wall-clock
// boundary configured via WithRotateEvery or WithRotateAt has passed, or on
// an explicit Rotate call. Compression and retention (WithMaxBackups,
// WithMaxAge, WithMaxTotalSize) run on a single background goroutine and
// never block writes.
//
// A Writer is safe for concurrent use by multiple goroutines. Like all
// single-writer rotation schemes, it assumes it is the only process writing
// to the file.
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

	millCh chan struct{}
	millWG sync.WaitGroup
}

var _ io.WriteCloser = (*Writer)(nil)

// New creates a Writer appending to filename, creating the file and any
// missing parent directories on the spot so configuration or permission
// problems surface here rather than at the first log record.
//
// If time-based rotation is configured and filename already exists, the
// current rotation period is recovered from the file's modification time; a
// leftover file last written in a previous period is rotated out immediately.
func New(filename string, opts ...Option) (*Writer, error) {
	if filename == "" {
		return nil, errors.New("logrotate: filename is required")
	}
	cfg := defaultConfig()
	for _, opt := range opts {
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
	w := &Writer{
		cfg:      cfg,
		filename: filename,
		dir:      filepath.Dir(filename),
		prefix:   base[:len(base)-len(ext)] + "-",
		ext:      ext,
		zipExts:  []string{".gz"},
		millCh:   make(chan struct{}, 1),
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

	w.millWG.Add(1)
	go w.millLoop()
	w.requestMill() // clean up leftovers from previous runs
	return w, nil
}

// Write implements io.Writer. It performs any due rotation, then writes p in
// full to the active file. Writes never trigger compression or retention work
// synchronously.
func (w *Writer) Write(p []byte) (n int, err error) {
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

	n, err = w.file.Write(p)
	w.size += int64(n)

	if err == nil && w.cfg.maxSize > 0 && w.size >= w.cfg.maxSize {
		// The file is full (or an oversized record blew past the limit);
		// rotate eagerly so it never lingers over the cap. The write itself
		// succeeded, so a rotation failure must not be returned as a write
		// failure — report it and retry on the next write.
		if rerr := w.rotateLocked(w.cfg.clock.Now()); rerr != nil {
			w.cfg.reportError(rerr)
		}
	}
	return n, err
}

// Close closes the active file and stops the background maintenance
// goroutine, waiting for in-flight compression or cleanup to finish. Close is
// idempotent. After Close, Write returns ErrClosed.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	err := w.closeFileLocked()
	w.mu.Unlock()

	// No rotation can signal the mill anymore: every sender checks w.closed
	// under w.mu before sending.
	close(w.millCh)
	w.millWG.Wait()
	return err
}

// Sync flushes the active file to stable storage, satisfying
// zapcore.WriteSyncer. It is a no-op when no file is open.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
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

// openExistingOrNewLocked opens the log file for appending, creating it if
// absent. When time-based rotation is enabled and the existing file was last
// written in a previous period, it is rotated out first so stale content
// never receives new records.
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

// openNewLocked creates the log file, assuming the path is free. prev, when
// non-nil, is the file just rotated out; the new file inherits its
// permissions (unless WithFileMode is set) and, on Linux, its owner.
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
		file.Close()
		return fmt.Errorf("logrotate: set log file mode: %w", err)
	}
	if prev != nil {
		if err := chown(w.filename, prev); err != nil {
			w.cfg.reportError(fmt.Errorf("logrotate: preserve log file owner: %w", err))
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
