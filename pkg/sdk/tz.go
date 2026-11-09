package sdk

import (
	"errors"
	"fmt"
	"strings"
	"time"

	// Embed the IANA timezone database so containers without zoneinfo still
	// resolve names such as America/Chicago (plan §3.6).
	_ "time/tzdata"
)

const (
	layoutDateTimeSpace = "2006-01-02 15:04"
	layoutDateTimeT     = "2006-01-02T15:04"
	layoutClock         = "15:04"
	// layoutDateTimeOffsetNoSeconds accepts RFC3339-shaped values that omit
	// seconds, e.g. 2026-11-08T09:00-07:00. Full RFC3339 is tried first.
	layoutDateTimeOffsetNoSeconds = "2006-01-02T15:04Z07:00"
)

var weekdayNames = map[string]time.Weekday{
	"Sunday":    time.Sunday,
	"Monday":    time.Monday,
	"Tuesday":   time.Tuesday,
	"Wednesday": time.Wednesday,
	"Thursday":  time.Thursday,
	"Friday":    time.Friday,
	"Saturday":  time.Saturday,
}

// ToUTC parses value in the named IANA timezone and returns the corresponding
// instant in UTC. now is the reference for layouts that omit a date: "15:04"
// means today in the user's zone, and "Monday 15:04" means the next occurrence
// of that weekday and time. An unknown timezone is an error.
func ToUTC(value, timezone string, now time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("sdk: load timezone %q: %w", timezone, err)
	}
	return toUTC(value, loc, now)
}

// toUTC is the location-based form used by ConvertArgs, whose contract passes
// an already-loaded *time.Location.
func toUTC(value string, loc *time.Location, now time.Time) (time.Time, error) {
	if loc == nil {
		return time.Time{}, errors.New("sdk: nil location")
	}

	v := strings.TrimSpace(value)
	if v == "" {
		return time.Time{}, errors.New("sdk: empty datetime")
	}

	// 1. RFC3339 carries its own offset; the instant is independent of loc.
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(layoutDateTimeOffsetNoSeconds, v); err == nil {
		return t.UTC(), nil
	}

	// 2. Absolute date and time, with a space or a T separator.
	if t, err := time.ParseInLocation(layoutDateTimeSpace, v, loc); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.ParseInLocation(layoutDateTimeT, v, loc); err == nil {
		return t.UTC(), nil
	}

	// 3. Clock only: today in the user's timezone.
	if t, err := time.ParseInLocation(layoutClock, v, loc); err == nil {
		localNow := now.In(loc)
		return time.Date(
			localNow.Year(), localNow.Month(), localNow.Day(),
			t.Hour(), t.Minute(), 0, 0, loc,
		).UTC(), nil
	}

	// 4. Weekday and clock: the next occurrence, strictly after now.
	if t, ok := parseWeekdayClock(v, loc, now); ok {
		return t.UTC(), nil
	}

	return time.Time{}, fmt.Errorf("sdk: unrecognised datetime %q", value)
}

// parseWeekdayClock handles "Monday 15:04". The weekday is relative to now in
// loc; a time later today stays today, otherwise it rolls forward a week.
func parseWeekdayClock(value string, loc *time.Location, now time.Time) (time.Time, bool) {
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return time.Time{}, false
	}

	weekday, ok := weekdayNames[fields[0]]
	if !ok {
		return time.Time{}, false
	}

	clock, err := time.Parse(layoutClock, fields[1])
	if err != nil {
		return time.Time{}, false
	}

	localNow := now.In(loc)
	daysAhead := (int(weekday) - int(localNow.Weekday()) + 7) % 7
	candidate := time.Date(
		localNow.Year(), localNow.Month(), localNow.Day(),
		clock.Hour(), clock.Minute(), 0, 0, loc,
	).AddDate(0, 0, daysAhead)

	if !candidate.After(localNow) {
		candidate = candidate.AddDate(0, 0, 7)
	}
	return candidate, true
}
