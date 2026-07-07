package logrotate

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// skipOnWindows skips tests that assert POSIX-only semantics: permission
// bits (Windows only models a read-only flag) and renaming open files.
func skipOnWindows(t *testing.T, reason string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(reason)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// baseTime is mid-period for both hourly and daily schedules.
var baseTime = time.Date(2026, 3, 14, 10, 30, 0, 0, time.UTC)

func newTestWriter(t *testing.T, opts ...Option) (*Writer, string, *fakeClock) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.log")
	clk := newFakeClock(baseTime)
	w, err := New(path, append([]Option{WithClock(clk)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, path, clk
}

func mustWrite(t *testing.T, w *Writer, s string) {
	t.Helper()
	n, err := w.Write([]byte(s))
	if err != nil || n != len(s) {
		t.Fatalf("Write(%q) = %d, %v; want %d, nil", s, n, err, len(s))
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(b)
}

// backups returns the base names of all backup files (everything in dir
// except the active file), sorted lexically.
func listDir(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			names = append(names, e.Name())
		}
	}
	return names
}

// drainMill stops the writer and runs one synchronous maintenance pass so
// tests can assert on final disk state deterministically.
func drainMill(t *testing.T, w *Writer) {
	t.Helper()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	w.millOnce()
}

func TestNewCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "app.log")
	w, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = w.Close() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("log file not created: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 && runtime.GOOS != "windows" {
		t.Errorf("mode = %v, want 0600", got)
	}
	if w.Filename() != filepath.Clean(path) {
		t.Errorf("Filename() = %q", w.Filename())
	}
}

func TestNewValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	cases := map[string]func() (*Writer, error){
		"empty filename":     func() (*Writer, error) { return New("") },
		"negative max size":  func() (*Writer, error) { return New(path, WithMaxSize(-1)) },
		"negative backups":   func() (*Writer, error) { return New(path, WithMaxBackups(-1)) },
		"negative age":       func() (*Writer, error) { return New(path, WithMaxAge(-time.Hour)) },
		"negative total":     func() (*Writer, error) { return New(path, WithMaxTotalSize(-1)) },
		"interval too small": func() (*Writer, error) { return New(path, WithRotateEvery(time.Millisecond)) },
		"interval too large": func() (*Writer, error) { return New(path, WithRotateEvery(25*time.Hour)) },
		"bad rotate-at":      func() (*Writer, error) { return New(path, WithRotateAt("24:00")) },
		"nil location":       func() (*Writer, error) { return New(path, WithLocation(nil)) },
		"bad time format":    func() (*Writer, error) { return New(path, WithBackupTimeFormat("2006/01/02")) },
		"bad file mode":      func() (*Writer, error) { return New(path, WithFileMode(os.ModeSticky|0o600)) },
		"bad compressor ext": func() (*Writer, error) {
			return New(path, WithCompressor(extCompressor{GzipCompressor{}, "gz"}))
		},
		"tmp compressor ext": func() (*Writer, error) {
			return New(path, WithCompressor(extCompressor{GzipCompressor{}, ".tmp"}))
		},
	}
	for name, mk := range cases {
		if w, err := mk(); err == nil {
			_ = w.Close()
			t.Errorf("%s: New succeeded, want error", name)
		}
	}
}

func TestNewRejectsNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "app.log")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if w, err := New(target); err == nil {
		_ = w.Close()
		t.Fatal("New on a directory succeeded, want error")
	}
	// The mistake must not mutate the filesystem: no rename, no new file.
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory was mutated: %v, %v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "app.log" {
		t.Errorf("directory tree mutated: %v", entries)
	}
}

func TestWriteAppends(t *testing.T) {
	w, path, _ := newTestWriter(t)
	mustWrite(t, w, "hello ")
	mustWrite(t, w, "world\n")
	if got := readFile(t, path); got != "hello world\n" {
		t.Errorf("content = %q", got)
	}
}

func TestNewAppendsToExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = w.Close() }()
	mustWrite(t, w, "+new")
	if got := readFile(t, path); got != "old+new" {
		t.Errorf("content = %q", got)
	}
}

func TestSizeRotation(t *testing.T) {
	w, path, _ := newTestWriter(t, WithMaxSize(10))
	mustWrite(t, w, "aaaaaa") // 6 bytes, stays
	mustWrite(t, w, "bbbbbb") // would exceed: rotate first
	drainMill(t, w)

	names := listDir(t, path)
	if len(names) != 1 {
		t.Fatalf("backups = %v, want one", names)
	}
	if got := readFile(t, filepath.Join(filepath.Dir(path), names[0])); got != "aaaaaa" {
		t.Errorf("backup content = %q", got)
	}
	if got := readFile(t, path); got != "bbbbbb" {
		t.Errorf("active content = %q", got)
	}
}

func TestSizeRotationExactFill(t *testing.T) {
	w, path, _ := newTestWriter(t, WithMaxSize(10))
	mustWrite(t, w, "aaaaabbbbb") // exactly at the cap: rotated out eagerly
	drainMill(t, w)

	if names := listDir(t, path); len(names) != 1 {
		t.Fatalf("backups = %v, want one", names)
	}
	if got := readFile(t, path); got != "" {
		t.Errorf("active content = %q, want empty", got)
	}
}

func TestOversizedWriteSucceeds(t *testing.T) {
	w, path, _ := newTestWriter(t, WithMaxSize(10))
	huge := strings.Repeat("x", 25)
	mustWrite(t, w, huge) // larger than the cap: written in full, then isolated
	mustWrite(t, w, "next")
	drainMill(t, w)

	names := listDir(t, path)
	if len(names) != 1 {
		t.Fatalf("backups = %v, want one", names)
	}
	if got := readFile(t, filepath.Join(filepath.Dir(path), names[0])); got != huge {
		t.Errorf("backup = %q, want the oversized record", got)
	}
	if got := readFile(t, path); got != "next" {
		t.Errorf("active content = %q", got)
	}
}

func TestMaxSizeZeroDisablesSizeRotation(t *testing.T) {
	w, path, _ := newTestWriter(t, WithMaxSize(0))
	mustWrite(t, w, strings.Repeat("x", 1<<12))
	mustWrite(t, w, strings.Repeat("y", 1<<12))
	if names := listDir(t, path); len(names) != 0 {
		t.Errorf("backups = %v, want none", names)
	}
}

func TestIntervalRotation(t *testing.T) {
	w, path, clk := newTestWriter(t, WithRotateEvery(time.Hour))
	mustWrite(t, w, "first")
	clk.set(time.Date(2026, 3, 14, 10, 59, 59, 0, time.UTC))
	mustWrite(t, w, "-still")
	clk.set(time.Date(2026, 3, 14, 11, 0, 0, 0, time.UTC)) // on the boundary
	mustWrite(t, w, "second")
	drainMill(t, w)

	backup := filepath.Join(filepath.Dir(path), "app-2026-03-14T11-00-00.000.log")
	if got := readFile(t, backup); got != "first-still" {
		t.Errorf("backup stamped with boundary: %q, files: %v", got, listDir(t, path))
	}
	if got := readFile(t, path); got != "second" {
		t.Errorf("active content = %q", got)
	}
}

func TestIntervalRotationSkipsIdlePeriods(t *testing.T) {
	w, path, clk := newTestWriter(t, WithRotateEvery(time.Hour))
	mustWrite(t, w, "a")
	clk.advance(5 * time.Hour) // five boundaries pass with no writes
	mustWrite(t, w, "b")
	drainMill(t, w)

	names := listDir(t, path)
	if len(names) != 1 {
		t.Fatalf("backups = %v, want exactly one (no empty catch-up files)", names)
	}
	// Stamp is the boundary that ended the file's period, not the write time.
	if names[0] != "app-2026-03-14T11-00-00.000.log" {
		t.Errorf("backup name = %q", names[0])
	}
}

func TestRotateAt(t *testing.T) {
	w, path, clk := newTestWriter(t, WithRotateAt("15:30", "03:00"))
	mustWrite(t, w, "morning")
	clk.set(time.Date(2026, 3, 14, 15, 29, 0, 0, time.UTC))
	mustWrite(t, w, "-noon")
	clk.set(time.Date(2026, 3, 14, 16, 0, 0, 0, time.UTC))
	mustWrite(t, w, "evening")
	drainMill(t, w)

	backup := filepath.Join(filepath.Dir(path), "app-2026-03-14T15-30-00.000.log")
	if got := readFile(t, backup); got != "morning-noon" {
		t.Errorf("backup = %q, files: %v", got, listDir(t, path))
	}
}

func TestWithLocationAlignsBoundariesAndStamps(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)
	// baseTime 10:30 UTC is 18:30 in UTC+8; the daily boundary anchored at
	// UTC+8 midnight is 16:00 UTC.
	w, path, clk := newTestWriter(t, WithRotateEvery(24*time.Hour), WithLocation(zone))
	mustWrite(t, w, "day one")
	clk.set(time.Date(2026, 3, 14, 15, 59, 0, 0, time.UTC))
	mustWrite(t, w, "!")
	if names := listDir(t, path); len(names) != 0 {
		t.Fatalf("rotated before the zone-local midnight: %v", names)
	}
	clk.set(time.Date(2026, 3, 14, 16, 0, 0, 0, time.UTC))
	mustWrite(t, w, "day two")
	drainMill(t, w)

	// The stamp is the boundary rendered in the configured zone.
	backup := filepath.Join(filepath.Dir(path), "app-2026-03-15T00-00-00.000.log")
	if got := readFile(t, backup); got != "day one!" {
		t.Errorf("backup = %q, files: %v", got, listDir(t, path))
	}
}

func TestRestartRecoversPeriodFromModTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("yesterday"), 0o600); err != nil {
		t.Fatal(err)
	}
	lastWrite := time.Date(2026, 3, 13, 23, 40, 0, 0, time.UTC)
	if err := os.Chtimes(path, lastWrite, lastWrite); err != nil {
		t.Fatal(err)
	}

	clk := newFakeClock(time.Date(2026, 3, 14, 1, 0, 0, 0, time.UTC))
	w, err := New(path, WithClock(clk), WithRotateEvery(24*time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mustWrite(t, w, "today")
	drainMill(t, w)

	backup := filepath.Join(filepath.Dir(path), "app-2026-03-14T00-00-00.000.log")
	if got := readFile(t, backup); got != "yesterday" {
		t.Errorf("stale file not rotated out at startup: %q, files: %v", got, listDir(t, path))
	}
	if got := readFile(t, path); got != "today" {
		t.Errorf("active content = %q", got)
	}
}

func TestRestartKeepsFileInSamePeriod(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("early"), 0o600); err != nil {
		t.Fatal(err)
	}
	lastWrite := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, lastWrite, lastWrite); err != nil {
		t.Fatal(err)
	}

	clk := newFakeClock(baseTime) // 10:30 same day
	w, err := New(path, WithClock(clk), WithRotateEvery(24*time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = w.Close() }()
	mustWrite(t, w, "-late")

	if names := listDir(t, path); len(names) != 0 {
		t.Errorf("backups = %v, want none", names)
	}
	if got := readFile(t, path); got != "early-late" {
		t.Errorf("active content = %q", got)
	}
}

func TestManualRotate(t *testing.T) {
	w, path, _ := newTestWriter(t)
	mustWrite(t, w, "before")
	if err := w.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	mustWrite(t, w, "after")
	drainMill(t, w)

	names := listDir(t, path)
	if len(names) != 1 {
		t.Fatalf("backups = %v", names)
	}
	if got := readFile(t, filepath.Join(filepath.Dir(path), names[0])); got != "before" {
		t.Errorf("backup = %q", got)
	}
	if got := readFile(t, path); got != "after" {
		t.Errorf("active = %q", got)
	}
}

func TestRotateEmptyFileIsNoop(t *testing.T) {
	w, path, clk := newTestWriter(t, WithRotateEvery(time.Hour))
	if err := w.Rotate(); err != nil { // freshly created, empty
		t.Fatal(err)
	}
	if names := listDir(t, path); len(names) != 0 {
		t.Fatalf("empty rotation produced backups: %v", names)
	}

	// A time boundary passing over an empty file must not produce one either.
	clk.advance(3 * time.Hour)
	mustWrite(t, w, "first")
	if names := listDir(t, path); len(names) != 0 {
		t.Fatalf("boundary over empty file produced backups: %v", names)
	}

	// With content, rotation works as usual.
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	if names := listDir(t, path); len(names) != 1 {
		t.Fatalf("backups = %v, want one", names)
	}
	if got := readFile(t, path); got != "" {
		t.Errorf("active file = %q, want empty after rotation", got)
	}
}

func TestBackupNameCollisionGetsSequence(t *testing.T) {
	w, path, _ := newTestWriter(t) // clock never advances
	mustWrite(t, w, "one")
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, w, "two")
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, w, "three")
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	drainMill(t, w)

	dir := filepath.Dir(path)
	ts := baseTime.Format(defaultTimeFormat)
	for name, want := range map[string]string{
		"app-" + ts + ".log":   "one",
		"app-" + ts + ".1.log": "two",
		"app-" + ts + ".2.log": "three",
	} {
		if got := readFile(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s = %q, want %q (files: %v)", name, got, want, listDir(t, path))
		}
	}
}

func TestMaxBackups(t *testing.T) {
	w, path, clk := newTestWriter(t, WithMaxBackups(2))
	for i := range 5 {
		mustWrite(t, w, fmt.Sprintf("gen-%d", i))
		if err := w.Rotate(); err != nil {
			t.Fatal(err)
		}
		clk.advance(time.Minute)
	}
	drainMill(t, w)

	names := listDir(t, path)
	if len(names) != 2 {
		t.Fatalf("backups = %v, want 2", names)
	}
	// The two newest generations survive.
	dir := filepath.Dir(path)
	got := readFile(t, filepath.Join(dir, names[0])) + "," + readFile(t, filepath.Join(dir, names[1]))
	if got != "gen-3,gen-4" {
		t.Errorf("surviving backups = %q, want newest two", got)
	}
}

func TestMaxAge(t *testing.T) {
	w, path, clk := newTestWriter(t, WithMaxAge(time.Hour))
	mustWrite(t, w, "old")
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	clk.advance(2 * time.Hour)
	mustWrite(t, w, "new")
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	drainMill(t, w)

	names := listDir(t, path)
	if len(names) != 1 {
		t.Fatalf("backups = %v, want only the fresh one", names)
	}
	if got := readFile(t, filepath.Join(filepath.Dir(path), names[0])); got != "new" {
		t.Errorf("surviving backup = %q", got)
	}
}

func TestMaxTotalSize(t *testing.T) {
	w, path, clk := newTestWriter(t, WithMaxTotalSize(250))
	for i := range 3 {
		mustWrite(t, w, strings.Repeat(fmt.Sprint(i), 100))
		if err := w.Rotate(); err != nil {
			t.Fatal(err)
		}
		clk.advance(time.Minute)
	}
	drainMill(t, w)

	names := listDir(t, path)
	if len(names) != 2 {
		t.Fatalf("backups = %v, want 2 (100+100 <= 250 < 300)", names)
	}
	for _, name := range names {
		if strings.Contains(readFile(t, filepath.Join(filepath.Dir(path), name)), "0") {
			t.Errorf("oldest backup should have been removed, found %s", name)
		}
	}
}

func TestCompress(t *testing.T) {
	w, path, _ := newTestWriter(t, WithCompress())
	payload := strings.Repeat("compress me\n", 100)
	mustWrite(t, w, payload)
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	drainMill(t, w)

	names := listDir(t, path)
	if len(names) != 1 || !strings.HasSuffix(names[0], ".log.gz") {
		t.Fatalf("backups = %v, want a single .log.gz", names)
	}
	f, err := os.Open(filepath.Join(filepath.Dir(path), names[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil || string(got) != payload {
		t.Errorf("decompressed = %d bytes, err %v; want original payload", len(got), err)
	}
}

func TestCompressCleansOrphanTmp(t *testing.T) {
	w, path, _ := newTestWriter(t, WithCompress())
	dir := filepath.Dir(path)
	orphan := filepath.Join(dir, "app-2026-03-14T09-00-00.000.log.gz.tmp")
	if err := os.WriteFile(orphan, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	drainMill(t, w)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphaned tmp file survived maintenance")
	}
}

func TestCompressFinishesInterruptedPass(t *testing.T) {
	w, path, _ := newTestWriter(t, WithCompress())
	dir := filepath.Dir(path)
	// Simulate a crash after the archive was published but before the plain
	// backup was removed.
	plain := filepath.Join(dir, "app-2026-03-14T09-00-00.000.log")
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write([]byte("data"))
	_ = zw.Close()
	if err := os.WriteFile(plain, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain+".gz", zipped.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	drainMill(t, w)

	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Errorf("plain backup should have been removed, files: %v", listDir(t, path))
	}
	if _, err := os.Stat(plain + ".gz"); err != nil {
		t.Errorf("existing archive should have been kept: %v", err)
	}
}

func TestCustomCompressor(t *testing.T) {
	w, path, _ := newTestWriter(t, WithCompressor(extCompressor{GzipCompressor{}, ".gzip"}))
	mustWrite(t, w, "custom")
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	drainMill(t, w)

	names := listDir(t, path)
	if len(names) != 1 || !strings.HasSuffix(names[0], ".log.gzip") {
		t.Errorf("backups = %v, want .log.gzip", names)
	}
}

// extCompressor overrides the extension of an underlying Compressor.
type extCompressor struct {
	Compressor
	ext string
}

func (c extCompressor) Extension() string { return c.ext }

type failingCompressor struct{}

func (failingCompressor) Compress(io.Writer, io.Reader) error { return errors.New("boom") }
func (failingCompressor) Extension() string                   { return ".fz" }

func TestErrorHandlerReceivesBackgroundErrors(t *testing.T) {
	var mu sync.Mutex
	var errs []error
	handler := func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() }

	w, path, _ := newTestWriter(t, WithCompressor(failingCompressor{}), WithErrorHandler(handler))
	mustWrite(t, w, "payload")
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	drainMill(t, w)

	mu.Lock()
	defer mu.Unlock()
	if len(errs) == 0 {
		t.Fatal("error handler never called for failed compression")
	}
	// The uncompressed backup must survive a failed compression.
	names := listDir(t, path)
	if len(names) != 1 || !strings.HasSuffix(names[0], ".log") {
		t.Errorf("backups = %v, want the plain backup intact", names)
	}
}

// TestMaxTotalSizeEnforcedWhenCompressionFails pins down the quota semantics
// under compression failure: the quota reflects actual on-disk bytes, so a
// backup left uncompressed by a failing compressor counts at its full size
// and the disk cap still holds. Deferring enforcement would let a persistent
// compression failure (full disk, broken compressor) defeat the one option
// whose job is bounding disk usage.
func TestMaxTotalSizeEnforcedWhenCompressionFails(t *testing.T) {
	var mu sync.Mutex
	var errs []error
	handler := func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() }

	w, path, clk := newTestWriter(t,
		WithCompressor(failingCompressor{}),
		WithMaxTotalSize(250),
		WithErrorHandler(handler),
	)
	for i := range 3 {
		mustWrite(t, w, strings.Repeat(fmt.Sprint(i), 100))
		if err := w.Rotate(); err != nil {
			t.Fatal(err)
		}
		clk.advance(time.Minute)
	}
	drainMill(t, w)

	names := listDir(t, path)
	if len(names) != 2 {
		t.Fatalf("backups = %v, want quota enforced at uncompressed sizes (2 files)", names)
	}
	for _, name := range names {
		if strings.HasSuffix(name, ".fz") {
			t.Errorf("backup %s unexpectedly compressed", name)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) == 0 {
		t.Error("compression failures were not reported")
	}
}

func TestCustomBackupTimeFormat(t *testing.T) {
	w, path, _ := newTestWriter(t, WithBackupTimeFormat("2006-01-02"), WithMaxBackups(2))
	for _, s := range []string{"one", "two", "three"} {
		mustWrite(t, w, s)
		if err := w.Rotate(); err != nil {
			t.Fatal(err)
		}
	}
	drainMill(t, w)

	names := listDir(t, path)
	if len(names) != 2 {
		t.Fatalf("backups = %v, want 2 (day-precision names with sequences)", names)
	}
	dir := filepath.Dir(path)
	got := readFile(t, filepath.Join(dir, names[0])) + "," + readFile(t, filepath.Join(dir, names[1]))
	if got != "two,three" {
		t.Errorf("survivors = %q, want the two newest by sequence", got)
	}
}

func TestForeignFilesUntouched(t *testing.T) {
	w, path, _ := newTestWriter(t, WithMaxBackups(1))
	dir := filepath.Dir(path)
	foreign := []string{
		"other.log",
		"app-notatimestamp.log",
		"app-2026-03-14T09-00-00.000.txt", // wrong extension
		"unrelated-2026-03-14T09-00-00.000.log",
	}
	for _, name := range foreign {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, w, "x")
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	drainMill(t, w)

	for _, name := range foreign {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("foreign file %s was touched: %v", name, err)
		}
	}
}

func TestCloseIdempotentAndWriteAfterClose(t *testing.T) {
	w, _, _ := newTestWriter(t)
	mustWrite(t, w, "x")
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := w.Write([]byte("y")); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close = %v, want ErrClosed", err)
	}
	if err := w.Rotate(); !errors.Is(err, ErrClosed) {
		t.Errorf("Rotate after Close = %v, want ErrClosed", err)
	}
	if err := w.Reopen(); !errors.Is(err, ErrClosed) {
		t.Errorf("Reopen after Close = %v, want ErrClosed", err)
	}
	if err := w.Sync(); err != nil {
		t.Errorf("Sync after Close = %v, want nil", err)
	}
}

func TestReopenAfterExternalMove(t *testing.T) {
	skipOnWindows(t, "Windows cannot rename a file held open by the writer; external rotators are a unix workflow")
	w, path, _ := newTestWriter(t)
	mustWrite(t, w, "before")
	moved := path + ".moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := w.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	mustWrite(t, w, "after")

	if got := readFile(t, moved); got != "before" {
		t.Errorf("moved file = %q", got)
	}
	if got := readFile(t, path); got != "after" {
		t.Errorf("reopened file = %q", got)
	}
}

func TestSync(t *testing.T) {
	w, _, _ := newTestWriter(t)
	mustWrite(t, w, "x")
	if err := w.Sync(); err != nil {
		t.Errorf("Sync: %v", err)
	}
}

func TestFileModeOption(t *testing.T) {
	skipOnWindows(t, "POSIX permission bits are not meaningful on Windows")
	umask := setUmask(0o027)
	defer setUmask(umask)

	w, path, _ := newTestWriter(t, WithFileMode(0o644))
	mustWrite(t, w, "x")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %v, want 0644 despite umask", got)
	}
}

func TestRotationInheritsMode(t *testing.T) {
	skipOnWindows(t, "POSIX permission bits are not meaningful on Windows")
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil { // WriteFile mode is umask-masked
		t.Fatal(err)
	}
	w, err := New(path, WithClock(newFakeClock(baseTime)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Rotate(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("rotated file mode = %v, want inherited 0640", got)
	}
}

func TestConcurrentWrites(t *testing.T) {
	w, path, _ := newTestWriter(t, WithMaxSize(512))
	const goroutines, writes = 8, 100
	line := strings.Repeat("z", 19) + "\n"

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range writes {
				if _, err := w.Write([]byte(line)); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	drainMill(t, w)

	var total int64
	dir := filepath.Dir(path)
	for _, name := range append(listDir(t, path), filepath.Base(path)) {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
		// No torn lines: every file is a whole number of records.
		if info.Size()%int64(len(line)) != 0 {
			t.Errorf("%s holds a partial record (size %d)", name, info.Size())
		}
	}
	if want := int64(goroutines * writes * len(line)); total != want {
		t.Errorf("total bytes = %d, want %d", total, want)
	}
}

func TestParseBackupName(t *testing.T) {
	w, _, _ := newTestWriter(t, WithCompress())
	ts := baseTime.Format(defaultTimeFormat)
	cases := []struct {
		name       string
		ok         bool
		seq        int
		compressed bool
	}{
		{"app-" + ts + ".log", true, 0, false},
		{"app-" + ts + ".log.gz", true, 0, true},
		{"app-" + ts + ".3.log", true, 3, false},
		{"app-" + ts + ".3.log.gz", true, 3, true},
		{"app.log", false, 0, false},
		{"app-.log", false, 0, false},
		{"app-" + ts + ".txt", false, 0, false},
		{"other-" + ts + ".log", false, 0, false},
		{"app-" + ts + ".0.log", false, 0, false}, // sequences start at 1
		{"app-" + ts + ".-1.log", false, 0, false},
	}
	for _, c := range cases {
		stamp, seq, compressed, ok := w.parseBackupName(c.name)
		if ok != c.ok {
			t.Errorf("parseBackupName(%q) ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if !stamp.Equal(baseTime) || seq != c.seq || compressed != c.compressed {
			t.Errorf("parseBackupName(%q) = %v, %d, %v", c.name, stamp, seq, compressed)
		}
	}
}

// BenchmarkRawFile is the floor: a bare O_APPEND file write with no locking
// or rotation logic. The gap between this and BenchmarkWrite is what the
// package costs per record.
func BenchmarkRawFile(b *testing.B) {
	f, err := os.OpenFile(filepath.Join(b.TempDir(), "raw.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	line := []byte(strings.Repeat("x", 127) + "\n")
	b.SetBytes(int64(len(line)))
	b.ResetTimer()
	for range b.N {
		if _, err := f.Write(line); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWrite(b *testing.B) {
	w, err := New(filepath.Join(b.TempDir(), "bench.log"), WithMaxSize(1*GB))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	line := []byte(strings.Repeat("x", 127) + "\n")
	b.SetBytes(int64(len(line)))
	b.ResetTimer()
	for range b.N {
		if _, err := w.Write(line); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteWithTimeRotation(b *testing.B) {
	w, err := New(filepath.Join(b.TempDir(), "bench.log"),
		WithMaxSize(1*GB), WithRotateEvery(24*time.Hour))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	line := []byte(strings.Repeat("x", 127) + "\n")
	b.SetBytes(int64(len(line)))
	b.ResetTimer()
	for range b.N {
		if _, err := w.Write(line); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteParallel(b *testing.B) {
	w, err := New(filepath.Join(b.TempDir(), "bench.log"), WithMaxSize(1*GB))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	line := []byte(strings.Repeat("x", 127) + "\n")
	b.SetBytes(int64(len(line)))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := w.Write(line); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
