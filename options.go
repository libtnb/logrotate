package logrotate

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"
)

// Byte size units for WithMaxSize and WithMaxTotalSize.
const (
	KB int64 = 1 << 10
	MB int64 = 1 << 20
	GB int64 = 1 << 30
)

// Day is a convenience duration for retention options, e.g.
// WithMaxAge(14*logrotate.Day). Retention compares plain durations; no
// calendar or DST semantics are implied.
const Day = 24 * time.Hour

const (
	defaultMaxSize    int64 = 100 * MB
	defaultTimeFormat       = "2006-01-02T15-04-05.000"
	defaultFileMode         = fs.FileMode(0o600)
	dirMode                 = fs.FileMode(0o755)
)

// Clock supplies the current time. The default clock uses time.Now; tests can
// substitute a fake via WithClock.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Option configures a Writer. Options are applied by New and validated
// together; an invalid combination makes New return an error instead of
// degrading silently at runtime.
type Option func(*config)

type config struct {
	maxSize      int64
	maxBackups   int
	maxAge       time.Duration
	maxTotalSize int64

	rotateEvery time.Duration
	rotateAtRaw []string
	rotateAt    []dayTime

	compressor Compressor

	loc        *time.Location
	timeFormat string
	fileMode   fs.FileMode

	errorHandler func(error)
	clock        Clock
}

func defaultConfig() config {
	return config{
		maxSize:    defaultMaxSize,
		timeFormat: defaultTimeFormat,
		loc:        time.UTC,
		clock:      systemClock{},
	}
}

// WithMaxSize sets the maximum size, in bytes, the log file may reach before
// being rotated. The KB, MB and GB constants help readability, e.g.
// WithMaxSize(64*logrotate.MB). A value of 0 disables size-based rotation.
// The default is 100*MB.
//
// A single record larger than the limit is still written in full (rejecting
// log records would lose data); it gets a file of its own, which is rotated
// out immediately after the write.
func WithMaxSize(n int64) Option {
	return func(c *config) { c.maxSize = n }
}

// WithMaxBackups limits how many rotated backups are retained; the oldest are
// removed first. 0, the default, keeps all backups. A backup and its
// compressed form count as one.
func WithMaxBackups(n int) Option {
	return func(c *config) { c.maxBackups = n }
}

// WithMaxAge removes backups whose rotation timestamp is older than d. 0, the
// default, disables age-based removal.
func WithMaxAge(d time.Duration) Option {
	return func(c *config) { c.maxAge = d }
}

// WithMaxTotalSize caps the combined size, in bytes, of all retained backups;
// the oldest are removed until the rest fit. The active log file does not
// count against the cap, so worst-case disk usage is roughly
// maxSize + maxTotalSize. 0, the default, disables the cap.
//
// The cap is strict and reflects actual bytes on disk: if even the newest
// backup exceeds it on its own, that backup is removed too, and a backup
// whose compression failed counts at its uncompressed size until compression
// succeeds.
func WithMaxTotalSize(n int64) Option {
	return func(c *config) { c.maxTotalSize = n }
}

// WithRotateEvery rotates the log on wall-clock boundaries every d, anchored
// at midnight in the configured location (UTC unless WithLocation is set):
// with d of one hour rotation happens on the hour, with 24h at midnight. The final interval
// of a day is shortened when d does not divide 24h evenly, so every day has
// the same boundary sequence. d must be between 1s and 24h; for calendar
// times of day use WithRotateAt instead.
//
// Boundaries are checked when writes occur, so an idle writer does not create
// empty files; the first write after a boundary triggers the rotation, and
// the backup is stamped with the boundary time rather than the write time.
// Across restarts the current period is recovered from the log file's
// modification time: a leftover file whose last write falls in a previous
// period is rotated out before it can receive new records.
func WithRotateEvery(d time.Duration) Option {
	return func(c *config) { c.rotateEvery = d }
}

// WithRotateAt rotates the log daily at the given wall-clock times, each in
// "HH:MM" 24-hour form (e.g. "00:00", "03:30"). Times are interpreted in the
// configured location (UTC unless WithLocation is set) and may be combined
// with WithRotateEvery; the earliest upcoming boundary wins. Repeated options
// accumulate.
//
// The same lazy-boundary semantics as WithRotateEvery apply.
func WithRotateAt(times ...string) Option {
	return func(c *config) { c.rotateAtRaw = append(c.rotateAtRaw, times...) }
}

// WithCompress gzip-compresses rotated backups in the background. Compression
// is atomic: a backup is written to a temporary file and renamed into place,
// so a crash never leaves a half-written archive behind.
func WithCompress() Option {
	return WithCompressor(GzipCompressor{})
}

// WithCompressor compresses rotated backups with a custom Compressor, for
// example a zstd implementation. Passing nil disables compression (the
// default).
func WithCompressor(comp Compressor) Option {
	return func(c *config) { c.compressor = comp }
}

// WithLocation sets the time zone for backup timestamps and time-based
// rotation boundaries: WithLocation(time.Local) follows the host zone, and
// any fixed zone works regardless of where the process runs. The default is
// UTC.
func WithLocation(loc *time.Location) Option {
	return func(c *config) { c.loc = loc }
}

// WithBackupTimeFormat sets the time layout embedded in backup file names.
// The default is "2006-01-02T15-04-05.000". The layout is validated by New:
// it must survive a format/parse round-trip and may not produce path
// separators. A layout coarser than the rotation frequency is fine; colliding
// names get a numeric sequence suffix (name-2006-01-02.1.log).
func WithBackupTimeFormat(layout string) Option {
	return func(c *config) { c.timeFormat = layout }
}

// WithFileMode sets the permission bits for newly created log files. The mode
// is applied exactly, unaffected by the process umask. When unset, a new file
// inherits the permissions of the file it replaces, or 0o600 if there is
// none.
func WithFileMode(mode fs.FileMode) Option {
	return func(c *config) { c.fileMode = mode }
}

// WithErrorHandler registers fn to receive errors from background maintenance
// (compression, retention cleanup) and from deferred rotations that could not
// be reported through a Write return value. fn is called from the background
// goroutine and must not call back into the Writer. Without a handler such
// errors are dropped.
func WithErrorHandler(fn func(error)) Option {
	return func(c *config) { c.errorHandler = fn }
}

// WithClock substitutes the time source, letting tests drive rotation
// deterministically.
func WithClock(clock Clock) Option {
	return func(c *config) { c.clock = clock }
}

func (c *config) validate() error {
	if c.maxSize < 0 {
		return errors.New("logrotate: max size must not be negative")
	}
	if c.maxBackups < 0 {
		return errors.New("logrotate: max backups must not be negative")
	}
	if c.maxAge < 0 {
		return errors.New("logrotate: max age must not be negative")
	}
	if c.maxTotalSize < 0 {
		return errors.New("logrotate: max total size must not be negative")
	}
	if c.rotateEvery != 0 && (c.rotateEvery < time.Second || c.rotateEvery > 24*time.Hour) {
		return fmt.Errorf("logrotate: rotate interval %v out of range [1s, 24h]", c.rotateEvery)
	}
	for _, s := range c.rotateAtRaw {
		dt, err := parseDayTime(s)
		if err != nil {
			return err
		}
		c.rotateAt = append(c.rotateAt, dt)
	}
	slices.SortFunc(c.rotateAt, compareDayTime)
	c.rotateAt = slices.CompactFunc(c.rotateAt, func(a, b dayTime) bool {
		return compareDayTime(a, b) == 0
	})
	if err := validateTimeFormat(c.timeFormat); err != nil {
		return err
	}
	if c.fileMode&^fs.ModePerm != 0 {
		return fmt.Errorf("logrotate: file mode %v contains non-permission bits", c.fileMode)
	}
	if c.compressor != nil {
		ext := c.compressor.Extension()
		if len(ext) < 2 || ext[0] != '.' || ext == tmpSuffix || strings.ContainsAny(ext, `/\`) {
			return fmt.Errorf("logrotate: invalid compressor extension %q", ext)
		}
	}
	if c.loc == nil {
		return errors.New("logrotate: location must not be nil")
	}
	if c.clock == nil {
		c.clock = systemClock{}
	}
	return nil
}

// validateTimeFormat rejects layouts that cannot name backups unambiguously:
// the formatted timestamp must parse back and re-format to the same string,
// and must not contain path separators.
func validateTimeFormat(layout string) error {
	if layout == "" {
		return errors.New("logrotate: backup time format must not be empty")
	}
	ref := time.Date(2015, 6, 21, 17, 48, 39, 123456789, time.UTC)
	s := ref.Format(layout)
	if s == "" || strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("logrotate: backup time format %q produces invalid file names", layout)
	}
	parsed, err := time.ParseInLocation(layout, s, time.UTC)
	if err != nil {
		return fmt.Errorf("logrotate: backup time format %q does not round-trip: %w", layout, err)
	}
	if parsed.Format(layout) != s {
		return fmt.Errorf("logrotate: backup time format %q does not round-trip", layout)
	}
	return nil
}

func (c *config) location() *time.Location {
	return c.loc
}

func (c *config) reportError(err error) {
	if c.errorHandler != nil && err != nil {
		c.errorHandler(err)
	}
}
