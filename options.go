package logrotate

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Binary byte units for WithMaxSize and WithMaxTotalSize.
const (
	// KB is 1024 bytes.
	KB int64 = 1 << 10
	// MB is 1024 KB.
	MB int64 = 1 << 20
	// GB is 1024 MB.
	GB int64 = 1 << 30
)

// Day is 24 hours, a convenience for WithMaxAge and WithRotateEvery. It has
// no calendar or DST semantics: 7*Day is always 168 hours.
const Day = 24 * time.Hour

const (
	defaultMaxSize    int64 = 100 * MB
	defaultTimeFormat       = "2006-01-02T15-04-05.000"
	defaultFileMode         = fs.FileMode(0o600)
	dirMode                 = fs.FileMode(0o755)
)

// Clock supplies the wall time used for rotation schedules, backup names and
// age-based retention; the default reads time.Now. Replace it with WithClock
// in tests to drive rotation without sleeping. The Writer calls Now under
// its write lock and, concurrently, from the maintenance goroutine, so
// implementations must be safe for concurrent use. The location of the
// returned time does not matter; it is converted to the WithLocation zone.
type Clock interface {
	// Now returns the current wall time.
	Now() time.Time
}

// systemClock is the default Clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Option configures a Writer. New applies the options in the order given, so
// a later option overrides an earlier one for the same setting (WithRotateAt
// accumulates instead), then validates the complete set before touching the
// file system; an invalid value or a nil Option makes New fail with a
// descriptive error.
type Option func(*config)

// WithMaxSize rotates the active file before a write would push it past n
// bytes and right after a write fills it. The default is 100*MB; zero
// disables size-based rotation, which suits an external rotator used with
// Reopen. A single record larger than n is written in full and rotated out
// on its own. New rejects a negative n.
func WithMaxSize(n int64) Option {
	return func(c *config) { c.maxSize = n }
}

// WithMaxBackups keeps only the n newest backups, ordered by timestamp and
// then by sequence; older ones are removed on the next maintenance pass.
// Zero, the default, keeps every backup. New rejects a negative n.
func WithMaxBackups(n int) Option {
	return func(c *config) { c.maxBackups = n }
}

// WithMaxAge removes backups whose timestamp is older than d at the time of
// a maintenance pass, measured with the configured Clock; the file
// modification time is not consulted. Zero, the default, disables age-based
// retention. New rejects a negative d.
func WithMaxAge(d time.Duration) Option {
	return func(c *config) { c.maxAge = d }
}

// WithMaxTotalSize removes the oldest backups until the on-disk size of the
// remaining ones fits within n bytes. The cap is applied after compression,
// so archives count at their compressed size and a backup whose compression
// failed counts in full; the active file is not counted. Zero, the default,
// disables the cap. New rejects a negative n.
func WithMaxTotalSize(n int64) Option {
	return func(c *config) { c.maxTotalSize = n }
}

// WithRotateEvery rotates at wall-clock boundaries every d, anchored at
// midnight in the WithLocation zone: 6*time.Hour rotates at 00:00, 06:00,
// 12:00 and 18:00. The last interval of a day is cut at the next midnight, so
// 24*time.Hour rotates at midnight even across a daylight-saving change.
// Boundaries are evaluated on writes: the first write at or after a boundary
// rotates first and the backup carries the boundary time; an idle period
// produces no empty backup. New rejects d outside [1s, 24h]; zero, the
// default, disables interval rotation.
func WithRotateEvery(d time.Duration) Option {
	return func(c *config) { c.rotateEvery = d }
}

// WithRotateAt rotates daily at the given "HH:MM" times of day in the
// WithLocation zone; hours and minutes may have one or two digits. Calls
// accumulate, duplicates collapse, and the times combine with WithRotateEvery,
// whichever boundary comes first. Like interval boundaries they are evaluated
// on writes. New rejects a time that is not HH:MM with hour 0-23 and minute
// 0-59.
func WithRotateAt(times ...string) Option {
	return func(c *config) { c.rotateAtRaw = append(c.rotateAtRaw, times...) }
}

// WithCompress compresses backups with gzip at the default level on the
// maintenance goroutine; it is WithCompressor(GzipCompressor{}). The package
// documentation describes how archives are staged and published.
func WithCompress() Option {
	return WithCompressor(GzipCompressor{})
}

// WithCompressor compresses backups with comp, serially on the maintenance
// goroutine; Compressor documents the contract. Nil, the default, disables
// compression. Backups ending in ".gz" are always recognised, so switching
// away from WithCompress keeps earlier archives under retention. New rejects
// a typed-nil comp and an Extension that does not start with ".", is shorter
// than two characters, equals ".tmp" or contains a path separator.
// Implementations should honor cancellation so Shutdown deadlines work.
func WithCompressor(comp Compressor) Option {
	return func(c *config) { c.compressor = comp }
}

// WithLocation sets the time zone in which schedule boundaries are computed
// and backup timestamps are rendered; the default is UTC. Backup names are
// parsed back in the same zone, so changing it for an existing directory
// shifts how earlier backups are ordered and aged. New rejects a nil loc.
func WithLocation(loc *time.Location) Option {
	return func(c *config) { c.loc = loc }
}

// WithBackupTimeFormat sets the time layout used for the timestamp in backup
// names; the default "2006-01-02T15-04-05.000" has millisecond precision. A
// coarser layout such as "2006-01-02" makes rotations within one period
// share a timestamp and be told apart by the sequence suffix instead. New
// rejects an empty layout, one that contains a path separator, and one whose
// output does not parse back to the same string, because backup names must
// be recognised and ordered again after a restart.
func WithBackupTimeFormat(layout string) Option {
	return func(c *config) { c.timeFormat = layout }
}

// WithFileMode sets the permission bits of the active file whenever the
// Writer creates it, applied exactly regardless of the umask. Without it a
// rotated file inherits the mode of the file it replaces and a brand-new
// file is created with 0o600. Archives always copy the mode of the backup
// they replace. New rejects a mode with bits outside fs.ModePerm.
func WithFileMode(mode fs.FileMode) Option {
	return func(c *config) { c.fileMode = mode }
}

// WithErrorHandler registers fn to be told about maintenance errors as they
// happen: compression, listing or removing backups, ownership preservation
// and rotations that failed after a successful write. Notifications are
// delivered serially on a dedicated goroutine, never under Writer locks, so
// a slow handler cannot delay writes, compression or retention; fn should
// still return promptly, and a panic in fn is contained. Delivery is
// advisory: when notifications arrive faster than fn drains them, the excess
// is dropped and summarised in one "error handler notifications omitted"
// error, and Close or Shutdown may return before the last notifications have
// been delivered. Close and Shutdown remain the authoritative record. fn may
// call Shutdown itself.
func WithErrorHandler(fn func(error)) Option {
	return func(c *config) { c.errorHandler = fn }
}

// WithClock replaces the wall-clock source used for schedules, backup names
// and age-based retention, so tests can drive rotation without sleeping. The
// default reads time.Now. New rejects a nil or typed-nil clock; Clock
// documents the concurrency requirement.
func WithClock(clock Clock) Option {
	return func(c *config) { c.clock = clock }
}

// config is the option set of a Writer, complete and validated by New.
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

// validate checks the complete option set and resolves the WithRotateAt
// strings into sorted, deduplicated times of day. New calls it once.
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
	intervalOutOfRange := c.rotateEvery < time.Second || c.rotateEvery > 24*time.Hour
	if c.rotateEvery != 0 && intervalOutOfRange {
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
	if isTypedNil(c.compressor) {
		return errors.New("logrotate: compressor is a typed nil")
	}
	if c.compressor != nil {
		ext := c.compressor.Extension()
		invalidExtension := len(ext) < 2 || ext[0] != '.' ||
			ext == tmpSuffix || strings.ContainsAny(ext, `/\`)
		if invalidExtension {
			return fmt.Errorf("logrotate: invalid compressor extension %q", ext)
		}
	}
	if c.loc == nil {
		return errors.New("logrotate: location must not be nil")
	}
	if c.clock == nil || isTypedNil(c.clock) {
		return errors.New("logrotate: clock must not be nil")
	}
	return nil
}

func (c *config) location() *time.Location {
	return c.loc
}

// validateTimeFormat rejects layouts that cannot name backups unambiguously:
// the formatted timestamp must parse back and re-format to the same string,
// and must not contain path separators.
func validateTimeFormat(layout string) error {
	if layout == "" {
		return errors.New("logrotate: backup time format must not be empty")
	}
	ref := time.Date(
		2015,
		6,
		21,
		17,
		48,
		39,
		123456789,
		time.UTC,
	)
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

// isTypedNil reports whether value is a non-nil interface holding a nil
// pointer, map, slice, channel or func, which would pass a plain nil check
// and then panic when used.
func isTypedNil(value any) bool {
	if value == nil {
		return false
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
