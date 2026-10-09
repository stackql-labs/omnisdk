package sqlfn_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
)

// timeValues are the time values SQLite reads: dates, date-times with every separator, fraction and
// zone form, bare times, Julian days and Unix times as numbers and text, the range limits, and
// malformed values on each field.
var timeValues = []any{
	nil, "", "x",
	"2024-01-01", "2024-02-29", "2023-02-29", "2024-02-30", "2024-12-31", "2000-01-01", "1970-01-01",
	"0000-01-01", "-0001-01-01", "-4713-11-24", "-4713-11-23", "-4714-01-01", "9999-12-31", "10000-01-01",
	"2024-13-01", "2024-00-10", "2024-01-32", "2024-01-00", "24-01-01", "2024-1-01", "2024-01-1",
	" 2024-01-01", "2024-01-01 ", "2024-01-01x",
	"2024-01-01 12:34", "2024-01-01T12:34", "2024-01-01 12:34:56", "2024-01-01T12:34:56.789",
	"2024-01-01 12:34:56.9999", "2024-01-01 12:34:56.1234", "2024-01-01 12:34:56.", "2024-01-01  12:34:56",
	"2024-01-01 24:00", "2024-01-01 24:00:01", "2024-01-01 25:00", "2024-01-01 12:60", "2024-01-01 12:34:60",
	"2024-01-01 12:34:56Z", "2024-01-01 12:34:56z", "2024-01-01 12:34:56+05:30", "2024-01-01 12:34:56 -08:00",
	"2024-01-01 12:34+14:00", "2024-01-01 12:34+15:00", "2024-01-01 12:34+01:60", "2024-01-01 00:00-00:00",
	"2024-01-01 12:34:56+0530", "2024-01-31 23:59:59.999",
	"12:34", "12:34:56", "12:34:56.5", "00:00", "24:00", "23:59:59.999", "12:34Z", "12:34+01:00",
	"now ", "nowx", " now",
	int64(0), int64(1), int64(-1), int64(2451545), int64(5373484), int64(5373485), int64(1700000000),
	2451545.5, 2460375.0883, -0.5, 5373484.49, 1e20,
	"2451545", "2451545.5", " 2451545 ", "1e6", "0x10", "12abc",
	[]byte("2024-01-01"),
}

// dateModifiers are SQLite's modifiers with their boundary forms and their malformed ones.
var dateModifiers = []any{
	nil, "", "bogus",
	"+1 day", "-1 day", "+1 days", "1 day", "+1.5 days", "+0.25 day", "+1 DAY", " +1 day ", "+1  day",
	"+1 hour", "-36 hours", "+90 minutes", "+3600 seconds", "+0.5 second", "-1.25 seconds",
	"+1 month", "-1 month", "+13 months", "-13 months", "+1.5 months", "+1 year", "-1 year", "+0.5 years",
	"+5373485 days", "+176546 months", "+14713 years", "-14713 years",
	"+01:30", "-01:30", "01:30", "+01:30:15", "+01:30:15.5", "+24:00", "+12:60",
	"+0001-02-03", "-0001-02-03", "+0000-11-30", "+0001-12-01", "+0001-02-03 04:05:06", "+0001-02-03 04:05",
	"start of day", "start of month", "start of year", "START OF MONTH", "start of week",
	"weekday 0", "weekday 3", "weekday 6", "weekday 7", "weekday 1.5", "weekday -1", "weekday 2.0",
	"floor", "ceiling", "subsec", "subsecond", "utc", "localtime",
	"unixepoch", "julianday", "auto",
}

// strftimeFormats exercise every conversion, the unsupported ones, and a trailing %.
var strftimeFormats = []any{
	nil, "", "plain",
	"%Y-%m-%d %H:%M:%S", "%f", "%j %J %s", "%w %u", "%W %U %V %G %g",
	"%e|%k|%l|%I|%p|%P", "%F %T %R", "%%", "%", "x%", "%C", "%y", "%D", "%h", "%b", "%a", "%A", "%B", "%Z",
}

var dateFuncs = []string{"date", "time", "datetime", "julianday", "unixepoch"}

func TestSQLiteParityDateValues(t *testing.T) {
	var specs []parity
	for _, n := range dateFuncs {
		specs = append(specs, parity{name: n, arity: []int{1}, dom: timeValues})
	}
	checkParity(t, sqlfn.BuiltinsFor(sqlfn.SQLite), specs)
}

func TestSQLiteParityDateModifiers(t *testing.T) {
	var specs []parity
	for _, n := range dateFuncs {
		var args [][]any
		for _, v := range timeValues {
			for _, m := range dateModifiers {
				args = append(args, []any{v, m})
			}
		}
		specs = append(specs, parity{name: n, args: args})
	}
	checkParity(t, sqlfn.BuiltinsFor(sqlfn.SQLite), specs)
}

// chains are modifier sequences whose order matters: month arithmetic before floor or ceiling, the
// numeric reinterpretations only first, start-of with offsets, and subsec in any position.
var chains = [][]any{
	{"+1 month", "floor"}, {"+1 month", "ceiling"}, {"floor", "+1 month"}, {"-1 month", "floor"},
	{"+1 year", "floor"}, {"+1 year", "ceiling"}, {"+0001-01-00", "floor"}, {"+0000-01-00", "floor"},
	{"start of month", "+1 month", "-1 day"}, {"start of year", "weekday 1"}, {"weekday 0", "start of day"},
	{"unixepoch", "+1 day"}, {"+1 day", "unixepoch"}, {"julianday", "start of day"}, {"auto", "subsec"},
	{"subsec", "+1.5 seconds"}, {"+1.5 seconds", "subsec"}, {"utc", "localtime"}, {"start of day", "subsec"},
	{"+12:00", "+12:00"}, {"-1 day", "-1 day", "-1 day"},
}

var chainValues = []any{
	"2024-01-31", "2024-01-31 10:20:30.456", "2023-01-29", "2024-02-29", "2024-03-31 23:59:59",
	"0000-01-31", "9999-12-31", int64(1700000000), 1700000000.5, int64(2460000), "2460000.25", "12:00",
	"2024-01-31 10:00+02:00", int64(-1),
}

func TestSQLiteParityDateChains(t *testing.T) {
	var specs []parity
	for _, n := range dateFuncs {
		var args [][]any
		for _, v := range chainValues {
			for _, c := range chains {
				args = append(args, append([]any{v}, c...))
			}
		}
		specs = append(specs, parity{name: n, args: args})
	}
	checkParity(t, sqlfn.BuiltinsFor(sqlfn.SQLite), specs)
}

func TestSQLiteParityStrftime(t *testing.T) {
	var args [][]any
	values := append(append([]any{}, chainValues...), "2024-01-01", "2021-01-03", "2020-12-31", "2027-01-01",
		"2024-12-30", "2024-03-05 00:07:09.123", "2024-03-05 12:00", "2024-03-05 23:59:59.9995", "-0044-03-15",
		nil, "bad")
	for _, f := range strftimeFormats {
		for _, v := range values {
			args = append(args, []any{f, v}, []any{f, v, "subsec"}, []any{f, v, "+6 days"})
		}
	}
	checkParity(t, sqlfn.BuiltinsFor(sqlfn.SQLite), []parity{{name: "strftime", args: args}})
}

func TestSQLiteParityTimediff(t *testing.T) {
	values := []any{
		nil, "bad", "2024-03-05", "2023-01-01", "2024-01-31", "2024-02-29", "2023-02-28", "2024-03-01",
		"2024-03-05 10:20:30.456", "2024-03-05 10:20:30.457", "0000-01-01", "9999-12-31 23:59:59.999",
		"-0100-06-15", "2024-01-31 23:00", "2024-03-01 01:00", int64(2460000), "12:00",
	}
	var args [][]any
	for _, a := range values {
		for _, b := range values {
			args = append(args, []any{a, b})
		}
	}
	checkParity(t, sqlfn.BuiltinsFor(sqlfn.SQLite), []parity{{name: "timediff", args: args}})
}

// TestSQLiteParityNow checks the current-time forms. SQLite fixes the time once per statement, so each
// case reads it from the same statement as the call and runs the port on that same instant.
func TestSQLiteParityNow(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := sqlfn.BuiltinsFor(sqlfn.SQLite)
	type call struct {
		name string
		args []any
	}
	var calls []call
	for _, n := range dateFuncs {
		calls = append(calls, call{n, nil})
		for _, v := range []any{"now", "NOW", "Now", "subsec", "SUBSECOND"} {
			calls = append(calls, call{n, []any{v}}, call{n, []any{v, "start of day"}}, call{n, []any{v, "localtime"}},
				call{n, []any{v, "subsec"}}, call{n, []any{v, "utc"}}, call{n, []any{v, "+1 month", "floor"}})
		}
	}
	for _, n := range []string{"current_date", "current_time", "current_timestamp"} {
		calls = append(calls, call{n, nil})
	}
	for _, f := range []any{"%s", "%f %J", "%Y-%m-%d %H:%M:%f"} {
		calls = append(calls, call{"strftime", []any{f}}, call{"strftime", []any{f, "now"}}, call{"strftime", []any{f, "subsec"}})
	}
	for _, c := range calls {
		qs := strings.TrimSuffix(strings.Repeat("?, ", len(c.args)), ", ")
		var jd float64
		var want any
		expr := c.name + "(" + qs + ")"
		if strings.HasPrefix(c.name, "current_") {
			expr = c.name // a keyword in SQLite's grammar
		}
		if err := db.QueryRow("select julianday('now'), "+expr, c.args...).Scan(&jd, &want); err != nil {
			t.Fatal(err)
		}
		iJD := int64(jd*86400000 + 0.5)
		restore := sqlfn.SetNow(func() int64 { return iJD })
		fn, _ := cat.Get(c.name)
		got, err := fn.Call(c.args)
		restore()
		if err != nil || !same(want, got) {
			t.Errorf("%s(%s): want %s, got %s (%v)", c.name, showArgs(c.args), show(want), show(got), err)
		}
	}
}
