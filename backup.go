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

// backup is one retained rotation result: the plain file, its compressed
// form, or (transiently, after an interrupted cleanup) both.
type backup struct {
	stamp time.Time
	seq   int
	files []backupFile
}

type backupFile struct {
	name       string // base name within the log directory
	size       int64
	compressed bool
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

// backupName returns a free backup path for a rotation stamped at t. On
// collision (a rotation already recorded for the same formatted timestamp) a
// numeric sequence is appended, so backups are never overwritten.
func (w *Writer) backupName(t time.Time) string {
	ts := t.In(w.cfg.location()).Format(w.cfg.timeFormat)
	name := filepath.Join(w.dir, w.prefix+ts+w.ext)
	for seq := 1; w.backupExists(name); seq++ {
		name = filepath.Join(w.dir, fmt.Sprintf("%s%s.%d%s", w.prefix, ts, seq, w.ext))
	}
	return name
}

// backupExists reports whether name is taken in any of its forms: plain,
// compressed, or mid-compression.
func (w *Writer) backupExists(name string) bool {
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
func (w *Writer) listBackups() (backups []*backup, orphans []string, err error) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil, nil, fmt.Errorf("logrotate: read log directory: %w", err)
	}

	type key struct {
		unix int64
		seq  int
	}
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
		k := key{stamp.UnixNano(), seq}
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
