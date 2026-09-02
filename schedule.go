package logrotate

import (
	"cmp"
	"fmt"
	"strings"
	"time"
)

// Time-based rotation uses boundaries computed in the configured zone. The
// interval schedule divides each local day from midnight into slots of the
// interval length, measured in elapsed time; the slot that would run past the
// next midnight is cut there, so a 24h interval means calendar midnight even
// on a 23- or 25-hour daylight-saving day, and sub-day intervals keep even
// elapsed spacing through the transition. The rotate-at schedule takes the
// earliest configured time of day still ahead, or the first one tomorrow.
// With both configured the earlier boundary wins. Every boundary is strictly
// after the reference time, so a write exactly on a boundary rotates once.

// dayTime is a wall-clock time of day used by WithRotateAt.
type dayTime struct {
	hour, min int
}

// parseDayTime accepts "HH:MM" with one or two digits per field and rejects
// anything else, including signs, spaces and seconds.
func parseDayTime(s string) (dayTime, error) {
	h, m, ok := strings.Cut(s, ":")
	hour, okH := parseTwoDigits(h)
	min, okM := parseTwoDigits(m)
	validTime := ok && okH && okM && hour <= 23 && min <= 59
	if !validTime {
		return dayTime{}, fmt.Errorf("logrotate: invalid rotate-at time %q, want \"HH:MM\"", s)
	}
	return dayTime{hour: hour, min: min}, nil
}

// parseTwoDigits parses one or two ASCII digits; strconv would also accept
// signs, underscores and longer input.
func parseTwoDigits(s string) (int, bool) {
	if len(s) < 1 || len(s) > 2 {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

func compareDayTime(a, b dayTime) int {
	if a.hour != b.hour {
		return cmp.Compare(a.hour, b.hour)
	}
	return cmp.Compare(a.min, b.min)
}

// nextRotation returns the earliest time-based rotation boundary strictly
// after t, or the zero time when no time-based rotation is configured.
func (c *config) nextRotation(t time.Time) time.Time {
	loc := c.location()
	t = t.In(loc)
	var next time.Time
	if c.rotateEvery > 0 {
		next = nextInterval(t, c.rotateEvery, loc)
	}
	if len(c.rotateAt) > 0 {
		at := nextDayTime(t, c.rotateAt, loc)
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next
}

// nextInterval anchors at local midnight. The next calendar midnight caps the
// final interval, so daily rotation remains correct across DST transitions.
func nextInterval(t time.Time, d time.Duration, loc *time.Location) time.Time {
	midnight := time.Date(
		t.Year(),
		t.Month(),
		t.Day(),
		0,
		0,
		0,
		0,
		loc,
	)
	k := t.Sub(midnight)/d + 1
	next := midnight.Add(time.Duration(k) * d)
	nextMidnight := midnight.AddDate(0, 0, 1)
	if time.Duration(k)*d >= 24*time.Hour || next.After(nextMidnight) {
		return nextMidnight
	}
	return next
}

// nextDayTime returns the earliest of the sorted times of day strictly after
// t, rolling over to the first one on the next day.
func nextDayTime(t time.Time, times []dayTime, loc *time.Location) time.Time {
	for _, dt := range times {
		candidate := time.Date(
			t.Year(),
			t.Month(),
			t.Day(),
			dt.hour,
			dt.min,
			0,
			0,
			loc,
		)
		if candidate.After(t) {
			return candidate
		}
	}
	next := t.AddDate(0, 0, 1)
	return time.Date(
		next.Year(),
		next.Month(),
		next.Day(),
		times[0].hour,
		times[0].min,
		0,
		0,
		loc,
	)
}
