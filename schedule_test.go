package logrotate

import (
	"testing"
	"time"
	_ "time/tzdata" // IANA zones for DST tests on platforms without system tzdata
)

func TestParseDayTime(t *testing.T) {
	valid := map[string]dayTime{
		"00:00": {0, 0},
		"3:07":  {3, 7},
		"03:30": {3, 30},
		"23:59": {23, 59},
	}
	for in, want := range valid {
		got, err := parseDayTime(in)
		if err != nil || got != want {
			t.Errorf("parseDayTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	invalid := []string{"", ":", "24:00", "12:60", "12", "12:", ":30", "1230", "12:30:00", "12:3O", "-1:30", "+2:30", " 12:30", "12:30 "}
	for _, in := range invalid {
		if _, err := parseDayTime(in); err == nil {
			t.Errorf("parseDayTime(%q) succeeded, want error", in)
		}
	}
}

func TestNextInterval(t *testing.T) {
	day := func(hh, mm, ss int) time.Time {
		return time.Date(2026, 3, 14, hh, mm, ss, 0, time.UTC)
	}
	cases := []struct {
		now  time.Time
		d    time.Duration
		want time.Time
	}{
		{day(10, 30, 0), time.Hour, day(11, 0, 0)},
		{day(11, 0, 0), time.Hour, day(12, 0, 0)}, // on a boundary: strictly after
		{day(23, 30, 0), time.Hour, day(24, 0, 0)},
		{day(10, 30, 0), 7 * time.Hour, day(14, 0, 0)},  // sequence 00,07,14,21
		{day(22, 0, 0), 7 * time.Hour, day(24, 0, 0)},   // 28h capped at next midnight
		{day(10, 30, 0), 24 * time.Hour, day(24, 0, 0)}, // daily
		{day(0, 0, 0), 24 * time.Hour, day(24, 0, 0)},
		{day(0, 0, 30), time.Minute, day(0, 1, 0)},
	}
	for _, c := range cases {
		if got := nextInterval(c.now, c.d, time.UTC); !got.Equal(c.want) {
			t.Errorf("nextInterval(%v, %v) = %v, want %v", c.now, c.d, got, c.want)
		}
	}
}

func TestNextIntervalDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}

	// Fall back (2026-11-01): the local day lasts 25 elapsed hours. Daily
	// rotation must still land on the next calendar midnight, not
	// midnight+24h (= 23:00 local).
	fallBackNoon := time.Date(2026, 11, 1, 12, 0, 0, 0, loc)
	if got, want := nextInterval(fallBackNoon, 24*time.Hour, loc), time.Date(2026, 11, 2, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("fall-back daily boundary = %v, want %v", got, want)
	}

	// Spring forward (2026-03-08): the local day lasts 23 elapsed hours.
	springNoon := time.Date(2026, 3, 8, 12, 0, 0, 0, loc)
	if got, want := nextInterval(springNoon, 24*time.Hour, loc), time.Date(2026, 3, 9, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("spring-forward daily boundary = %v, want %v", got, want)
	}

	// Sub-day boundaries keep even elapsed spacing through the repeated
	// hour of the fall-back transition.
	early := time.Date(2026, 11, 1, 0, 30, 0, 0, loc) // 00:30 EDT
	if got := nextInterval(early, time.Hour, loc); got.Sub(early) != 30*time.Minute {
		t.Errorf("hourly spacing across fall-back = %v, want 30m to the next boundary", got.Sub(early))
	}
}

func TestNextDayTime(t *testing.T) {
	times := []dayTime{{3, 0}, {15, 30}}
	day := func(d, hh, mm int) time.Time {
		return time.Date(2026, 3, d, hh, mm, 0, 0, time.UTC)
	}
	cases := []struct {
		now, want time.Time
	}{
		{day(14, 1, 0), day(14, 3, 0)},
		{day(14, 3, 0), day(14, 15, 30)}, // exactly on a mark: strictly after
		{day(14, 10, 0), day(14, 15, 30)},
		{day(14, 16, 0), day(15, 3, 0)}, // rolls over to next day
	}
	for _, c := range cases {
		if got := nextDayTime(c.now, times, time.UTC); !got.Equal(c.want) {
			t.Errorf("nextDayTime(%v) = %v, want %v", c.now, got, c.want)
		}
	}
}

func TestNextRotationCombined(t *testing.T) {
	cfg := defaultConfig()
	cfg.rotateEvery = time.Hour
	cfg.rotateAt = []dayTime{{10, 30}}

	now := time.Date(2026, 3, 14, 10, 5, 0, 0, time.UTC)
	want := time.Date(2026, 3, 14, 10, 30, 0, 0, time.UTC) // rotateAt wins over 11:00
	if got := cfg.nextRotation(now); !got.Equal(want) {
		t.Errorf("nextRotation = %v, want %v", got, want)
	}

	now = time.Date(2026, 3, 14, 10, 45, 0, 0, time.UTC)
	want = time.Date(2026, 3, 14, 11, 0, 0, 0, time.UTC) // interval wins
	if got := cfg.nextRotation(now); !got.Equal(want) {
		t.Errorf("nextRotation = %v, want %v", got, want)
	}

	plain := defaultConfig()
	if got := plain.nextRotation(now); !got.IsZero() {
		t.Errorf("nextRotation with no schedule = %v, want zero", got)
	}
}

func TestValidateTimeFormat(t *testing.T) {
	for _, layout := range []string{defaultTimeFormat, "2006-01-02", "20060102-150405", "2006-01-02_15-04-05"} {
		if err := validateTimeFormat(layout); err != nil {
			t.Errorf("validateTimeFormat(%q) = %v, want nil", layout, err)
		}
	}
	for _, layout := range []string{"", "2006/01/02", `2006\01\02`} {
		if err := validateTimeFormat(layout); err == nil {
			t.Errorf("validateTimeFormat(%q) succeeded, want error", layout)
		}
	}
}
