package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Backups live next to the active file and are named
//
//	<base>-<timestamp>[.<seq>]<ext>[<archive ext>][.tmp]
//
// where <base> and <ext> come from the active file name, <timestamp> is the
// rotation stamp rendered in the configured layout and zone, and <seq>
// starts at 1 for the second backup sharing a timestamp. When parsing, the
// bare form is tried before the sequence form so that a layout with
// fractional seconds is never mistaken for a sequence. A new sequence is one
// past the highest on disk for its timestamp, in any form, so sequences grow
// monotonically even after retention has removed earlier ones and the order
// listBackups derives from them stays correct; a rotation that repeats the
// previous timestamp probes the next sequence with Lstat instead of reading
// the directory again.

// backupFile is one file on disk belonging to a backup.
type backupFile struct {
	name       string // base name within the log directory
	size       int64
	compressed bool
}

// backup is one retained rotation result: the plain file, its compressed
// form, or (transiently, after an interrupted cleanup) both.
type backup struct {
	stamp time.Time
	seq   int
	files []backupFile
}

func (b *backup) size() int64 {
	var n int64
	for _, f := range b.files {
		n += f.size
	}
	return n
}

func (b *backup) compressed() bool {
	for _, f := range b.files {
		if f.compressed {
			return true
		}
	}
	return false
}

// backupName returns the backup path for a rotation stamped at t. When the
// formatted timestamp is already in use (a layout coarser than the rotation
// cadence, or two rotations inside one tick) a sequence suffix one past the
// highest in use is appended. Sequences grow monotonically within a timestamp
// even after retention has deleted earlier ones, so the newest-first order
// listBackups derives from them stays correct and no backup is overwritten.
func (w *Writer) backupName(t time.Time) string {
	ts := t.In(w.cfg.location()).Format(w.cfg.timeFormat)
	seq := w.nextSequence(ts)
	w.lastStamp, w.lastSeq = ts, seq
	if seq == 0 {
		return filepath.Join(w.dir, w.prefix+ts+w.ext)
	}
	return filepath.Join(w.dir, w.prefix+ts+"."+strconv.Itoa(seq)+w.ext)
}

// nextSequence returns the sequence for a new backup stamped ts: 0 when the
// timestamp is unused, else one past the highest sequence on disk in any form
// (plain, compressed, mid-compression). Repeated rotations within one stamp
// continue from the last allocation without rescanning the directory; a new
// stamp scans once so gaps left by retention, or by an earlier process, are
// never reused.
func (w *Writer) nextSequence(ts string) int {
	if ts == w.lastStamp && !w.backupTaken(ts, w.lastSeq+1) {
		return w.lastSeq + 1
	}
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		// Unreadable directory: probe upward from the last known sequence so
		// the rename that follows still never overwrites a backup.
		var seq int
		if ts == w.lastStamp {
			seq = w.lastSeq + 1
		}
		for w.backupTaken(ts, seq) {
			seq++
		}
		return seq
	}
	highest := -1
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), tmpSuffix)
		if seq, ok := w.sequenceOf(name, ts); ok && seq > highest {
			highest = seq
		}
	}
	return highest + 1
}

// sequenceOf reports the sequence of a backup file name stamped ts, in any
// recognised form, or false for other names.
func (w *Writer) sequenceOf(name, ts string) (int, bool) {
	rest, ok := strings.CutPrefix(name, w.prefix)
	if !ok {
		return 0, false
	}
	for _, ext := range w.zipExts {
		if s, found := strings.CutSuffix(rest, ext); found {
			rest = s
			break
		}
	}
	rest, ok = strings.CutSuffix(rest, w.ext)
	if !ok {
		return 0, false
	}
	if rest == ts {
		return 0, true
	}
	tail, ok := strings.CutPrefix(rest, ts+".")
	if !ok {
		return 0, false
	}
	seq, err := strconv.Atoi(tail)
	if err != nil || seq <= 0 {
		return 0, false
	}
	return seq, true
}

// backupTaken reports whether the backup stamped ts with sequence seq exists
// in any of its forms: plain, compressed, or mid-compression.
func (w *Writer) backupTaken(ts string, seq int) bool {
	name := filepath.Join(w.dir, w.prefix+ts+w.ext)
	if seq > 0 {
		name = filepath.Join(w.dir, w.prefix+ts+"."+strconv.Itoa(seq)+w.ext)
	}
	if _, err := os.Lstat(name); err == nil {
		return true
	}
	for _, ext := range w.zipExts {
		if _, err := os.Lstat(name + ext); err == nil {
			return true
		}
		if _, err := os.Lstat(name + ext + tmpSuffix); err == nil {
			return true
		}
	}
	return false
}

// listBackups scans the log directory and returns our backups sorted newest
// first, together with the names of orphaned temporary files left behind by
// an interrupted compression.
func (w *Writer) listBackups() ([]*backup, []string, error) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil, nil, fmt.Errorf("logrotate: read log directory: %w", err)
	}

	type key struct {
		unix int64
		seq  int
	}
	backups := []*backup{}
	orphans := []string{}
	byStamp := make(map[key]*backup)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if tmp, ok := strings.CutSuffix(name, tmpSuffix); ok {
			if _, _, _, ok := w.parseBackupName(tmp); ok {
				orphans = append(orphans, name)
			}
			continue
		}
		stamp, seq, compressed, ok := w.parseBackupName(name)
		if !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue // vanished between ReadDir and Info
		}
		k := key{unix: stamp.UnixNano(), seq: seq}
		b := byStamp[k]
		if b == nil {
			b = &backup{stamp: stamp, seq: seq}
			byStamp[k] = b
			backups = append(backups, b)
		}
		b.files = append(b.files, backupFile{name: name, size: info.Size(), compressed: compressed})
	}

	slices.SortFunc(backups, func(a, b *backup) int {
		if c := b.stamp.Compare(a.stamp); c != 0 {
			return c
		}
		return b.seq - a.seq
	})
	return backups, orphans, nil
}

// parseBackupName decodes the rotation timestamp, collision sequence and
// compression state from a file name produced by backupName, or reports
// ok=false for anything else.
func (w *Writer) parseBackupName(name string) (stamp time.Time, seq int, compressed bool, ok bool) {
	rest, hasPrefix := strings.CutPrefix(name, w.prefix)
	if !hasPrefix {
		return time.Time{}, 0, false, false
	}
	for _, ext := range w.zipExts {
		if s, found := strings.CutSuffix(rest, ext); found {
			rest, compressed = s, true
			break
		}
	}
	rest, hasExt := strings.CutSuffix(rest, w.ext)
	if !hasExt {
		return time.Time{}, 0, false, false
	}
	stamp, seq, ok = w.parseStamp(rest)
	return stamp, seq, compressed, ok
}

// parseStamp parses "<timestamp>" or "<timestamp>.<seq>". The bare form is
// tried first so a layout with fractional seconds (".000") is never mistaken
// for a sequence number.
func (w *Writer) parseStamp(s string) (time.Time, int, bool) {
	loc := w.cfg.location()
	if t, err := time.ParseInLocation(w.cfg.timeFormat, s, loc); err == nil {
		return t, 0, true
	}
	i := strings.LastIndexByte(s, '.')
	if i < 0 {
		return time.Time{}, 0, false
	}
	seq, err := strconv.Atoi(s[i+1:])
	if err != nil || seq <= 0 {
		return time.Time{}, 0, false
	}
	t, err := time.ParseInLocation(w.cfg.timeFormat, s[:i], loc)
	if err != nil {
		return time.Time{}, 0, false
	}
	return t, seq, true
}
