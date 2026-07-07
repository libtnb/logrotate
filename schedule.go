package logrotate

import (
	"cmp"
	"fmt"
	"strings"
	"time"
)

// dayTime is a wall-clock time of day used by WithRotateAt.
type dayTime struct {
	hour, min int
}

func parseDayTime(s string) (dayTime, error) {
	h, m, ok := strings.Cut(s, ":")
	hour, okH := parseTwoDigits(h)
	min, okM := parseTwoDigits(m)
	if !ok || !okH || !okM || hour > 23 || min > 59 {
		return dayTime{}, fmt.Errorf("logrotate: invalid rotate-at time %q, want \"HH:MM\"", s)
	}
	return dayTime{hour: hour, min: min}, nil
}

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

// nextInterval anchors interval boundaries at midnight in loc so that every
// day repeats the same boundary sequence (midnight, d, 2d, ...) regardless of
// process restarts. When d does not divide the day evenly the sequence is
// capped at the following midnight, shortening the last interval.
//
// The day's final boundary is always the calendar midnight, not "midnight
// plus 24h of elapsed time": on a DST fall-back day (25 wall-clock hours)
// daily rotation still happens at the next local midnight. Intermediate
// sub-day boundaries keep even elapsed spacing, so their wall-clock labels
// can shift by the DST offset for the remainder of a transition day.
func nextInterval(t time.Time, d time.Duration, loc *time.Location) time.Time {
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
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
		cand := time.Date(t.Year(), t.Month(), t.Day(), dt.hour, dt.min, 0, 0, loc)
		if cand.After(t) {
			return cand
		}
	}
	next := t.AddDate(0, 0, 1)
	return time.Date(next.Year(), next.Month(), next.Day(), times[0].hour, times[0].min, 0, 0, loc)
}
