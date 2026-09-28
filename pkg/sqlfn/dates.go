package sqlfn

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// timeValue is a moment in UTC.
type timeValue = time.Time

// now is the clock date functions read "now" from; replaceable in tests.
var now = time.Now

// dateFn is a SQLite date function: a time value, then modifiers applied in order.
func dateFn(render func(timeValue) any) func([]any) (any, error) {
	return strict(func(a []any) (any, error) {
		t, err := parseTime(text(a[0]), a[1:])
		if err != nil {
			return nil, err
		}
		for _, m := range a[1:] {
			if t, err = modify(t, strings.TrimSpace(text(m))); err != nil {
				return nil, err
			}
		}
		return render(t), nil
	})
}

var layouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05",
	"2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"}

// parseTime reads SQLite's time values: "now", an ISO-8601 string, or a number — a Julian day, or
// Unix seconds where a 'unixepoch' modifier follows.
func parseTime(s string, mods []any) (timeValue, error) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "now") {
		return now().UTC(), nil
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		for _, m := range mods {
			if strings.EqualFold(strings.TrimSpace(text(m)), "unixepoch") {
				sec := int64(f)
				return time.Unix(sec, int64((f-float64(sec))*1e9)).UTC(), nil
			}
		}
		return fromJulian(f), nil
	}
	return time.Time{}, fmt.Errorf("sqlfn: %q is not a time value", s)
}

// modify applies one SQLite modifier: ±N days/hours/minutes/seconds/months/years, "start of day",
// "start of month", "start of year", or "unixepoch" and "utc", which parsing already honoured.
func modify(t timeValue, m string) (timeValue, error) {
	lm := strings.ToLower(m)
	switch lm {
	case "unixepoch", "utc", "localtime", "auto":
		return t, nil
	case "start of day":
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
	case "start of month":
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC), nil
	case "start of year":
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC), nil
	}
	fields := strings.Fields(lm)
	if len(fields) != 2 {
		return t, fmt.Errorf("sqlfn: unsupported date modifier %q", m)
	}
	n, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return t, fmt.Errorf("sqlfn: unsupported date modifier %q", m)
	}
	switch strings.TrimSuffix(fields[1], "s") {
	case "day":
		return t.Add(time.Duration(n * 24 * float64(time.Hour))), nil
	case "hour":
		return t.Add(time.Duration(n * float64(time.Hour))), nil
	case "minute":
		return t.Add(time.Duration(n * float64(time.Minute))), nil
	case "second":
		return t.Add(time.Duration(n * float64(time.Second))), nil
	case "month":
		return t.AddDate(0, int(n), 0), nil
	case "year":
		return t.AddDate(int(n), 0, 0), nil
	}
	return t, fmt.Errorf("sqlfn: unsupported date modifier %q", m)
}

const unixEpochJulian = 2440587.5

func julian(t timeValue) float64 {
	return unixEpochJulian + float64(t.UnixNano())/float64(24*time.Hour)
}

func fromJulian(d float64) timeValue {
	return time.Unix(0, int64((d-unixEpochJulian)*float64(24*time.Hour))).UTC()
}
