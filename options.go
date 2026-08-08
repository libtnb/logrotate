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

// Byte size units for WithMaxSize and WithMaxTotalSize.
const (
	KB int64 = 1 << 10
	MB int64 = 1 << 20
	GB int64 = 1 << 30
)

// Day is 24 hours; it has no calendar or DST semantics.
const Day = 24 * time.Hour

const (
	defaultMaxSize    int64 = 100 * MB
	defaultTimeFormat       = "2006-01-02T15-04-05.000"
	defaultFileMode         = fs.FileMode(0o600)
	dirMode                 = fs.FileMode(0o755)
)

// Clock supplies wall time. The default uses time.Now.
type Clock interface {
	// Now returns the current wall time.
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Option configures a Writer. New validates the complete option set.
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

// WithMaxSize rotates at n bytes. Zero disables the limit; the default is
// 100*MB. A record larger than n is written in full before rotation.
func WithMaxSize(n int64) Option {
	return func(c *config) { c.maxSize = n }
}

// WithMaxBackups keeps the newest n backups. Zero keeps all backups.
func WithMaxBackups(n int) Option {
	return func(c *config) { c.maxBackups = n }
}

// WithMaxAge removes backups older than d. Zero disables age retention.
func WithMaxAge(d time.Duration) Option {
	return func(c *config) { c.maxAge = d }
}

// WithMaxTotalSize removes oldest backups until their on-disk size fits n.
// Zero disables the cap; the active file is not counted.
func WithMaxTotalSize(n int64) Option {
	return func(c *config) { c.maxTotalSize = n }
}

// WithRotateEvery rotates on midnight-anchored wall-clock boundaries. d must
// be between one second and 24 hours. Boundaries are evaluated on writes.
func WithRotateEvery(d time.Duration) Option {
	return func(c *config) { c.rotateEvery = d }
}

// WithRotateAt rotates daily at "HH:MM" times in the configured location.
// Calls accumulate and may be combined with WithRotateEvery.
func WithRotateAt(times ...string) Option {
	return func(c *config) { c.rotateAtRaw = append(c.rotateAtRaw, times...) }
}

// WithCompress enables atomic background gzip compression.
func WithCompress() Option {
	return WithCompressor(GzipCompressor{})
}

// WithCompressor installs a custom compressor. Nil disables compression.
// Implementations should honor cancellation so Shutdown deadlines work.
func WithCompressor(comp Compressor) Option {
	return func(c *config) { c.compressor = comp }
}

// WithLocation sets the time zone for schedules and backup names. Default UTC.
func WithLocation(loc *time.Location) Option {
	return func(c *config) { c.loc = loc }
}

// WithBackupTimeFormat sets the time layout in backup names. New rejects
// layouts that do not round-trip or that contain path separators.
func WithBackupTimeFormat(layout string) Option {
	return func(c *config) { c.timeFormat = layout }
}

// WithFileMode sets exact permission bits for new files. Otherwise rotation
// preserves the previous mode and brand-new files use 0o600.
func WithFileMode(mode fs.FileMode) Option {
	return func(c *config) { c.fileMode = mode }
}

// WithErrorHandler receives serialized maintenance-error notifications on a
// dedicated goroutine. The handler never runs under Writer locks and cannot
// delay writes, compression, or retention. It should return promptly; panics
// are contained. Notifications are advisory because overload is coalesced;
// Shutdown returns the authoritative bounded error record.
func WithErrorHandler(fn func(error)) Option {
	return func(c *config) { c.errorHandler = fn }
}

// WithClock replaces the time source. New rejects nil Clocks.
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

func defaultConfig() config {
	return config{
		maxSize:    defaultMaxSize,
		timeFormat: defaultTimeFormat,
		loc:        time.UTC,
		clock:      systemClock{},
	}
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
