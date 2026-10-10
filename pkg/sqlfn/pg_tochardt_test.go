package sqlfn_test

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
)

// pgToCharDTKeywords are the DCH keywords, upper and lower case.
var pgToCharDTKeywords = []string{
	"A.D.", "A.M.", "AD", "AM", "B.C.", "BC", "CC", "DAY", "DDD", "DD", "DY", "Day", "Dy", "D", "FF1", "FF2",
	"FF3", "FF4", "FF5", "FF6", "FX", "HH24", "HH12", "HH", "IDDD", "ID", "IW", "IYYY", "IYY", "IY", "I", "J",
	"MI", "MM", "MONTH", "MON", "MS", "Month", "Mon", "OF", "P.M.", "PM", "Q", "RM", "SSSSS", "SSSS", "SS",
	"TZH", "TZM", "TZ", "US", "WW", "W", "Y,YYY", "YYYY", "YYY", "YY", "Y", "a.d.", "a.m.", "ad", "am", "b.c.",
	"bc", "cc", "day", "ddd", "dd", "dy", "d", "ff1", "ff2", "ff3", "ff4", "ff5", "ff6", "fx", "hh24", "hh12",
	"hh", "iddd", "id", "iw", "iyyy", "iyy", "iy", "i", "j", "mi", "mm", "month", "mon", "ms", "p.m.", "pm",
	"q", "rm", "sssss", "ssss", "ss", "tz", "us", "ww", "w", "y,yyy", "yyyy", "yyy", "yy", "y",
}

// pgToCharDTPictures are whole pictures: separators, quoting, prefixes without keywords, case mixes.
var pgToCharDTPictures = []string{
	"", " ", "YYYY-MM-DD HH24:MI:SS.US", "Day, DD Month YYYY", "FMDay, FMDD FMMonth YYYY", "TMDay TMMonth",
	"DDth \"of\" Month", "\"YYYY\" YYYY", "\\\"YYYY", "\"a\\\"b\" YYYY", "FM", "FMx", "TMX", "YYYYTH", "HH:MI AM",
	"HH12:MI:SS pm", "IYYY-IW-ID", "YYYY-WW-D", "Q-CC-J", "MONTHMONTH", "Mon Dy", "rm RM FMrm", "TZ tz OF TZH:TZM",
	"é YYYY", "SSSSSS", "YYYYYYYY", "HHHH", "hH mI", "Dd", "DdD", "FMHH12th FMMIth", "MsUs", "FF7", "Y,YYYth",
	"FMY,YYY", "SP", "FMSP", "DDSP", "Y YY YYY YYYY", "IDDD-IYYY", "dAY", "yYyY", "TMday TMmon TMDAY TMMON",
}

func pgToCharDTFormats() []string {
	var out []string
	for _, k := range pgToCharDTKeywords {
		out = append(out, k, "FM"+k, k+"TH", k+"th", "FM"+k+"th", "TM"+k, k+"SP", "["+k+"]")
	}
	return append(out, pgToCharDTPictures...)
}

func pgTyped(t *testing.T, typ string, texts []string) []any {
	var out []any
	for _, s := range texts {
		v, err := sqlfn.PgTyped(typ, s)
		if err != nil {
			t.Fatalf("%s %q: %v", typ, s, err)
		}
		out = append(out, v)
	}
	return out
}

var (
	pgToCharDTTimestamps = []string{"2024-02-29 13:45:01.123456", "2000-01-01 00:00:00", "1999-12-31 23:59:59.999999",
		"0001-01-01 00:00:00", "0001-12-31 12:00:00 BC", "4713-11-24 00:00:00 BC", "294276-12-31 23:59:59.999999",
		"2021-01-03 00:00:00", "2020-12-31 12:00", "2008-12-29 01:02:03", "1900-02-28 11:30", "infinity", "-infinity",
		"2011-11-11 11:11:11.010101", "0044-03-15 12:00 BC", "2023-06-15 00:00:00.5", "2023-01-01", "2026-12-12 22:22",
		"10000-03-01 00:00", "0999-07-04 09:09:09.000009", "1001-01-01 00:00 BC"}
	pgToCharDTTimestamptzs = []string{"2024-02-29 13:45:01+05:30", "2000-01-01 00:00:00+00", "1999-12-31 23:59:59.999999-08",
		"0001-01-01 00:00:00+00 BC", "infinity", "-infinity", "2023-06-15 12:34:56.789+14", "2012-02-29 00:00:00-03:30"}
	pgToCharDTIntervals = []string{"0", "1 day", "25 hours", "-25 hours 30 minutes", "1 year 2 months 3 days 04:05:06.789",
		"-1 year -2 months", "12 months", "-12 months", "100 years", "-3 days 23:59:59.999999", "1000000 hours", "13 months",
		"-13 months", "-1 month", "11 months", "2147483647 hours", "-2147483647 hours", "178000000 years", "-0.000001 seconds",
		"12:00", "00:00:00.5", "3 years 15 days 13:00", "-12:30:00"}
)

// TestPgParityToCharDT runs to_char over timestamp, timestamptz and interval across every keyword
// with each modifier, and whole pictures.
func TestPgParityToCharDT(t *testing.T) {
	postgresImage(t)
	formats := pgToCharDTFormats()
	var cases []pgCase
	for _, vs := range [][]any{pgTyped(t, "timestamp", pgToCharDTTimestamps), pgTyped(t, "timestamptz", pgToCharDTTimestamptzs),
		pgTyped(t, "interval", pgToCharDTIntervals)} {
		for _, v := range vs {
			for _, f := range formats {
				cases = append(cases, pgCase{"to_char", []any{v, f}})
			}
		}
	}
	checkPg(t, cases)
}

// pgToTimestampCases are to_timestamp's inputs and pictures, separators, FX, out-of-range fields and
// conflicting conventions among them.
var pgToTimestampCases = [][2]string{
	{"2024-02-29 13:45:01", "YYYY-MM-DD HH24:MI:SS"}, {"2024 02 29", "YYYY MM DD"}, {"2024/02/29", "YYYY-MM-DD"},
	{"  2024-02-29", "YYYY-MM-DD"}, {"2024--02--29", "YYYY-MM-DD"}, {"2024-02-29", "YYYY--MM--DD"},
	{"2024-02-29", "FXYYYY-MM-DD"}, {"2024 -02-29", "FXYYYY-MM-DD"}, {"2024-2-29", "FXYYYY-MM-DD"},
	{"2024-2-29", "YYYY-MM-DD"}, {"20240229", "YYYYMMDD"}, {"2024229", "YYYYMMDD"}, {"240229", "YYMMDD"},
	{"69", "YY"}, {"70", "YY"}, {"519", "YYY"}, {"520", "YYY"}, {"999", "YYY"}, {"1", "Y"}, {"12345", "YYYY"},
	{"2024-13-01", "YYYY-MM-DD"}, {"2024-02-30", "YYYY-MM-DD"}, {"2023-02-29", "YYYY-MM-DD"}, {"2024-00-10", "YYYY-MM-DD"},
	{"2024-01-32", "YYYY-MM-DD"}, {"25:00", "HH24:MI"}, {"12:60", "HH24:MI"}, {"12:30:60", "HH24:MI:SS"},
	{"13:00", "HH12:MI"}, {"0:00", "HH12:MI"}, {"12:00 AM", "HH12:MI AM"}, {"12:00 PM", "HH12:MI PM"},
	{"01:00 pm", "HH:MI am"}, {"01:00 p.m.", "HH:MI a.m."}, {"01:00 xm", "HH:MI AM"}, {"11:00 PM", "HH24:MI PM"},
	{"2024-02-29 01:00 AM 02:00 PM", "YYYY-MM-DD HH:MI AM HH:MI PM"},
	{"February 29 2024", "Month DD YYYY"}, {"FEB 29 2024", "MON DD YYYY"}, {"feb 29 2024", "Mon DD YYYY"},
	{"Febr 29 2024", "Mon DD YYYY"}, {"Foo 29 2024", "Month DD YYYY"}, {"Thursday 2024-02-29", "Day YYYY-MM-DD"},
	{"Thu 2024-02-29", "Dy YYYY-MM-DD"}, {"thursday", "TMDay"}, {"MARCH", "TMMonth"},
	{"2024 060", "YYYY DDD"}, {"2024 366", "YYYY DDD"}, {"2023 366", "YYYY DDD"}, {"060", "DDD"}, {"2024 400", "YYYY DDD"},
	{"2024 10 3", "IYYY IW ID"}, {"2024 10", "IYYY IW"}, {"2024 100", "IYYY IDDD"}, {"2024 10 3", "YYYY IW ID"},
	{"2024 10 3", "IYYY WW D"}, {"2024 02 10", "IYYY MM IW"}, {"2024 10 3", "YYYY WW D"}, {"2024 2 3", "YYYY MM W"},
	{"2460370", "J"}, {"2460370 12", "J HH24"}, {"0", "J"},
	{"21 24", "CC YY"}, {"21", "CC"}, {"21 2024", "CC YYYY"}, {"5 24 BC", "CC YY BC"}, {"5 00 BC", "CC YY BC"},
	{"20 00", "CC YY"}, {"6 BC", "CC BC"},
	{"0044-03-15 BC", "YYYY-MM-DD BC"}, {"0044-03-15 B.C.", "YYYY-MM-DD B.C."}, {"0044-03-15 AD", "YYYY-MM-DD BC"},
	{"0044-03-15 ad", "YYYY-MM-DD AD"}, {"0000-01-01", "YYYY-MM-DD"}, {"-0044-03-15", "YYYY-MM-DD"},
	{"4714-11-24 BC", "YYYY-MM-DD BC"}, {"4713-11-24 BC", "YYYY-MM-DD BC"}, {"294277-01-01", "YYYY-MM-DD"},
	{"294276-12-31", "YYYY-MM-DD"},
	{"2,024", "Y,YYY"}, {"12,345", "Y,YYY"}, {"2024", "Y,YYY"}, {"2, 24", "Y,YYY"}, {"2,0245", "Y,YYY"}, {"-2,024", "Y,YYY"},
	{"12:34:56.789", "HH24:MI:SS.MS"}, {"12:34:56.7", "HH24:MI:SS.MS"}, {"12:34:56.78", "HH24:MI:SS.MS"},
	{"12:34:56.123456", "HH24:MI:SS.US"}, {"12:34:56.12", "HH24:MI:SS.US"}, {"12:34:56.1234567", "HH24:MI:SS.US"},
	{"12:34:56.123456", "HH24:MI:SS.FF1"}, {"12:34:56.1", "HH24:MI:SS.FF1"}, {"12:34:56.12", "HH24:MI:SS.FF2"},
	{"12:34:56.123", "HH24:MI:SS.FF3"}, {"12:34:56.1234", "HH24:MI:SS.FF4"}, {"12:34:56.12345", "HH24:MI:SS.FF5"},
	{"12:34:56.123456", "HH24:MI:SS.FF6"}, {"12:34:56.95", "HH24:MI:SS.FF1"}, {"12:34:56.5 123", "HH24:MI:SS.MS US"},
	{"45296", "SSSS"}, {"86399", "SSSSS"}, {"86400", "SSSS"}, {"45296 13", "SSSS HH24"},
	{"2024-02-29 13:45 +05:30", "YYYY-MM-DD HH24:MI TZH:TZM"}, {"2024-02-29 13:45 -05", "YYYY-MM-DD HH24:MI TZH"},
	{"2024-02-29 13:45-05", "YYYY-MM-DD HH24:MITZH"}, {"2024-02-29 13:45 05", "YYYY-MM-DD HH24:MI TZH"},
	{"2024-02-29 13:45 +16", "YYYY-MM-DD HH24:MI TZH"}, {"2024-02-29 13:45 +05:60", "YYYY-MM-DD HH24:MI TZH:TZM"},
	{"2024-02-29 13:45 30", "YYYY-MM-DD HH24:MI TZM"}, {"2024-02-29 13:45 -15:59", "YYYY-MM-DD HH24:MI TZH:TZM"},
	{"2024-02-29 13:45 UTC", "YYYY-MM-DD HH24:MI TZ"}, {"2024-02-29 +05", "YYYY-MM-DD OF"}, {"x", "tz"},
	{"2024 2025", "YYYY YYYY"}, {"2024 2024", "YYYY YYYY"}, {"Feb 02", "Mon MM"}, {"Feb 03", "Mon MM"},
	{"2024-02-29", "YYYY-MM-DDTH"}, {"29th Feb 2024", "DDth Mon YYYY"}, {"1st Feb 2024", "FMDDth Mon YYYY"},
	{"2024th", "YYYYTH"}, {"ab", "YYYY"}, {"", "YYYY"}, {"2024", ""}, {"", ""}, {"2024", "\"x\"YYYY"},
	{"x2024", "\"x\"YYYY"}, {"xx2024", "\"x\"YYYY"}, {"2024 x", "YYYY"}, {"2024", "YYYY MM DD"},
	{"9999999999", "YYYY"}, {"99999999999999999999", "FMYYYY"}, {"2147483648", "FMJ"}, {"-1", "FMDD"},
	{"+5", "FMDD"}, {"12345", "DD"}, {" 5", "FXDD"}, {"5", "DD"}, {"5 ", "DD"}, {"1-2", "DD-MM"}, {"1 2", "FXDD-MM"},
	{"2024 x 02", "YYYY x MM"}, {"2024xx02", "YYYYxMM"}, {"2024é02", "YYYYéMM"}, {"2024é02", "YYYY-MM"},
	{"XII 2024", "RM YYYY"}, {"ix 2024", "rm YYYY"}, {"VIII", "RM"}, {"V", "RM"}, {"XIII", "RM"}, {"Q", "RM"},
	{"3 2024", "Q YYYY"}, {"x 2024", "Q YYYY"}, {"2024 52", "YYYY WW"}, {"2024 54", "YYYY WW"},
	{"7", "ID"}, {"8", "ID"}, {"2024 1 7", "IYYY IW ID"}, {"2024 53 1", "IYYY IW ID"}, {"2024 1 0", "IYYY IW ID"},
	{"Monday 2024 10", "Day IYYY IW"}, {"2024 10 Monday", "IYYY IW Day"}, {"Sat 2024 10", "Dy IYYY IW"},
	{"2024 10 3 2024", "IYYY IW ID YYYY"}, {"2024 3", "YYYY ID"}, {"2024 100 2024", "IYYY IDDD YYYY"},
	{"2024 060 03", "YYYY DDD MM"}, {"2024 060 03 05", "YYYY DDD MM DD"}, {"12:00:00 2024", "HH24:MI:SS YYYY"},
	{"12", "HH"}, {"2024-02-29T13:45:01", "YYYY-MM-DD\"T\"HH24:MI:SS"}, {"2024-02-29T13:45:01", "YYYY-MM-DDTHH24:MI:SS"},
	{"2024-02-29 13:45:01.5", "YYYY-MM-DD HH24:MI:SS.FF2"}, {"1 2 3 4 5 6", "Y MM DD HH MI SS"},
	{"05/04/2024", "MM/DD/YYYY"}, {"5.4.24", "DD.MM.YY"}, {"2024 Feb", "YYYY FMMonth"}, {"2024 February", "YYYY MON"},
	{"20240229134501", "YYYYMMDDHH24MISS"}, {"2024022913", "YYYYMMDDHH24MISS"}, {"2024", "YYYYMMDD"},
	{"0001-01-01 BC", "YYYY-MM-DD BC"}, {"0000-01-01 BC", "YYYY-MM-DD BC"}, {"1 BC", "Y BC"},
}

// TestPgParityToTimestamp runs to_timestamp over hand-written cases and over to_char's output for
// each value and picture read back.
func TestPgParityToTimestamp(t *testing.T) {
	postgresImage(t)
	cat := pgCatalog(t)
	toChar, _ := cat.Get("to_char")
	var cases []pgCase
	for _, c := range pgToTimestampCases {
		cases = append(cases, pgCase{"to_timestamp", []any{c[0], c[1]}})
	}
	r := rand.New(rand.NewSource(7))
	formats := pgToCharDTFormats()
	for _, v := range pgTyped(t, "timestamptz", append(pgToCharDTTimestamps[:], pgToCharDTTimestamptzs...)) {
		for i := 0; i < 120; i++ {
			f := formats[r.Intn(len(formats))]
			if i%3 == 0 {
				f = "YYYY-MM-DD " + f
			}
			s, err := toChar.Call([]any{v, f})
			if err != nil || s == nil {
				continue
			}
			cases = append(cases, pgCase{"to_timestamp", []any{s.(string), f}})
		}
	}
	checkPg(t, cases)
}

// pgToTimestampRandom are to_timestamp calls on random inputs against random pictures, so that the
// parser's skipping, FX and field errors meet arbitrary text.
func pgToTimestampRandom() []pgCase {
	keys := []string{"YYYY", "MM", "DD", "HH24", "HH", "MI", "SS", "MS", "US", "FF3", "AM", "Mon", "Day", "DDD", "IW",
		"ID", "IYYY", "WW", "D", "J", "CC", "YY", "Y,YYY", "BC", "TZH", "TZM", "FX", "FM", "th", "-", " ", "/", ":", "x", "\"T\""}
	bits := []string{"2024", "02", "29", "13", "1", "45", "-", " ", "  ", "/", ":", ".", "x", "T", "Feb", "PM", "Thu",
		"BC", "+05", "999", "0", "12345", "a.m.", "Tue", "th", "+", "é"}
	r := rand.New(rand.NewSource(11))
	var cases []pgCase
	for i := 0; i < 6000; i++ {
		var f, s strings.Builder
		for n := 1 + r.Intn(6); n > 0; n-- {
			f.WriteString(keys[r.Intn(len(keys))])
		}
		for n := 1 + r.Intn(7); n > 0; n-- {
			s.WriteString(bits[r.Intn(len(bits))])
		}
		cases = append(cases, pgCase{"to_timestamp", []any{s.String(), f.String()}})
	}
	return cases
}

func TestPgParityToTimestampRandom(t *testing.T) {
	postgresImage(t)
	checkPg(t, pgToTimestampRandom())
}

// TestPgParityToCharDTErrors checks the catalogue fails with Postgres's message.
func TestPgParityToCharDTErrors(t *testing.T) {
	db := postgresImage(t)
	cat := pgCatalog(t)
	var cases []pgCase
	for _, c := range pgToTimestampCases {
		cases = append(cases, pgCase{"to_timestamp", []any{c[0], c[1]}})
	}
	for _, v := range pgTyped(t, "interval", pgToCharDTIntervals[:3]) {
		for _, k := range pgToCharDTKeywords {
			cases = append(cases, pgCase{"to_char", []any{v, k}})
		}
	}
	cases = append(cases, pgToTimestampRandom()...)
	compared := 0
	for _, c := range cases {
		fn, _ := cat.Get(c.name)
		var s *string
		werr := db.QueryRow("select " + pgCallSQL(c.name, c.args)).Scan(&s)
		_, gerr := fn.Call(c.args)
		if werr == nil || gerr == nil {
			continue
		}
		want := strings.TrimPrefix(werr.Error(), "ERROR: ")
		if i := strings.Index(want, " (SQLSTATE"); i >= 0 {
			want = want[:i]
		}
		compared++
		if want != gerr.Error() {
			t.Errorf("%s: want error %q, got %q", pgCallSQL(c.name, c.args), want, gerr.Error())
		}
	}
	t.Logf("%d of %d cases fail on both sides with the same message", compared, len(cases))
}
