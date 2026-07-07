package logrotate

import (
	"fmt"
	"testing"
	"time"
)

func fuzzWriter() *Writer {
	return &Writer{cfg: defaultConfig(), prefix: "app-", ext: ".log", zipExts: []string{".gz"}}
}

// FuzzParseBackupName asserts the parser never panics and never reports a
// non-positive sequence for arbitrary directory entries.
func FuzzParseBackupName(f *testing.F) {
	w := fuzzWriter()
	f.Add("app-2026-03-14T10-30-00.000.log")
	f.Add("app-2026-03-14T10-30-00.000.3.log.gz")
	f.Add("app-2026-03-14T10-30-00.000.log.gz.tmp")
	f.Add("app-....log.gz")
	f.Add("app.log")
	f.Add("")
	f.Fuzz(func(t *testing.T, name string) {
		_, seq, _, ok := w.parseBackupName(name)
		if ok && seq < 0 {
			t.Errorf("parseBackupName(%q) returned negative sequence %d", name, seq)
		}
	})
}

// FuzzStampRoundTrip asserts every name backupName could emit parses back to
// the same stamp and sequence.
func FuzzStampRoundTrip(f *testing.F) {
	w := fuzzWriter()
	f.Add(int64(1700000000), uint8(0), false)
	f.Add(int64(0), uint8(1), true)
	f.Fuzz(func(t *testing.T, sec int64, seq uint8, zipped bool) {
		if sec < 0 || sec > 4102444800 { // keep within year 0..2100
			t.Skip()
		}
		stamp := time.Unix(sec, 0).UTC()
		name := w.prefix + stamp.Format(w.cfg.timeFormat) + w.ext
		if seq > 0 {
			name = fmt.Sprintf("%s%s.%d%s", w.prefix, stamp.Format(w.cfg.timeFormat), seq, w.ext)
		}
		if zipped {
			name += ".gz"
		}
		got, gotSeq, gotZipped, ok := w.parseBackupName(name)
		if !ok || !got.Equal(stamp) || gotSeq != int(seq) || gotZipped != zipped {
			t.Errorf("round trip failed for %q: got %v seq=%d zipped=%v ok=%v",
				name, got, gotSeq, gotZipped, ok)
		}
	})
}
