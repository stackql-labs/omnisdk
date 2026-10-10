package sqlfn

import (
	"math"
	"unicode/utf8"
)

// The date and time functions of timestamp.c: now, to_timestamp(double precision), date_trunc and
// date_part.

// downcaseTruncateIdentifier is downcase_truncate_identifier in UTF8: ASCII folded, cut to
// NAMEDATALEN-1 bytes at a character boundary.
func downcaseTruncateIdentifier(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	if len(b) > 63 {
		n := 0
		for n < len(b) {
			_, size := utf8.DecodeRune(b[n:])
			if n+size > 63 {
				break
			}
			n += size
		}
		b = b[:n]
	}
	return string(b)
}

// pgFloat8Timestamptz is float8_timestamptz, to_timestamp(double precision).
func pgFloat8Timestamptz(seconds float64) (any, error) {
	if math.IsNaN(seconds) {
		return nil, pgErrorf("timestamp cannot be NaN")
	}
	if math.IsInf(seconds, -1) {
		return pgTimestamptz(pgDTNoBegin), nil
	}
	if math.IsInf(seconds, 1) {
		return pgTimestamptz(pgDTNoEnd), nil
	}
	if seconds < float64(pgSecsPerDay)*(pgDatetimeMinJulian-pgUnixEpochJDate) ||
		seconds >= float64(pgSecsPerDay)*(pgTimestampEndJulian-pgUnixEpochJDate) {
		return nil, pgErrorf("timestamp out of range: \"%s\"", pgFormatG(seconds))
	}
	seconds -= (pgPostgresEpochJDate - pgUnixEpochJDate) * pgSecsPerDay
	seconds = math.RoundToEven(seconds * float64(pgUsecsPerSec))
	r := int64(seconds)
	if !pgIsValidTimestamp(r) {
		return nil, pgErrorf("timestamp out of range: \"%s\"", pgFormatG(seconds))
	}
	return pgTimestamptz(r), nil
}

// pgFormatG is C's %g.
func pgFormatG(f float64) string { return formatFloat("%g", f) }

// truncTm is the shared fall-through of timestamp_trunc and timestamptz_trunc_internal; redotz
// reports a truncation to a day or coarser.
func truncTm(val int32, tm *pgTm, fsec *int64) (redotz, ok bool) {
	fallthroughFrom := func(from int32) {
		steps := []int32{dtkMillennium, dtkCentury, dtkDecade, dtkYear, dtkQuarter, dtkMonth, dtkDay, dtkHour, dtkMinute, dtkSecond}
		start := -1
		for i, s := range steps {
			if s == from {
				start = i
			}
		}
		for _, s := range steps[start:] {
			switch s {
			case dtkMillennium:
				if tm.year > 0 {
					tm.year = ((tm.year+999)/1000)*1000 - 999
				} else {
					tm.year = -((999-(tm.year-1))/1000)*1000 + 1
				}
			case dtkCentury:
				if tm.year > 0 {
					tm.year = ((tm.year+99)/100)*100 - 99
				} else {
					tm.year = -((99-(tm.year-1))/100)*100 + 1
				}
			case dtkDecade:
				if val != dtkMillennium && val != dtkCentury {
					if tm.year > 0 {
						tm.year = (tm.year / 10) * 10
					} else {
						tm.year = -((8 - (tm.year - 1)) / 10) * 10
					}
				}
			case dtkYear:
				tm.mon = 1
			case dtkQuarter:
				tm.mon = (3 * ((tm.mon - 1) / 3)) + 1
			case dtkMonth:
				tm.mday = 1
			case dtkDay:
				tm.hour = 0
				redotz = true
			case dtkHour:
				tm.min = 0
			case dtkMinute:
				tm.sec = 0
			case dtkSecond:
				*fsec = 0
			}
		}
	}
	switch val {
	case dtkWeek:
		woy := date2isoweek(tm.year, tm.mon, tm.mday)
		if woy >= 52 && tm.mon == 1 {
			tm.year--
		}
		if woy <= 1 && tm.mon == pgMonthsPerYear {
			tm.year++
		}
		tm.year, tm.mon, tm.mday = j2date(isoweek2j(tm.year, woy))
		tm.hour, tm.min, tm.sec = 0, 0, 0
		*fsec = 0
		return true, true
	case dtkMillennium, dtkCentury, dtkDecade, dtkYear, dtkQuarter, dtkMonth, dtkDay, dtkHour, dtkMinute, dtkSecond:
		fallthroughFrom(val)
		return redotz, true
	case dtkMillisec:
		*fsec = (*fsec / 1000) * 1000
		return false, true
	case dtkMicrosec:
		return false, true
	}
	return false, false
}

func isoweek2j(year, week int32) int32 {
	day4 := date2j(year, 1, 4)
	day0 := j2day(day4 - 1)
	return ((week - 1) * 7) + (day4 - day0)
}

// date2isoweek is date2isoweek.
func date2isoweek(year, mon, mday int32) int32 {
	dayn := date2j(year, mon, mday)
	day4 := date2j(year, 1, 4)
	day0 := j2day(day4 - 1)
	if dayn < day4-day0 {
		day4 = date2j(year-1, 1, 4)
		day0 = j2day(day4 - 1)
	}
	result := (dayn-(day4-day0))/7 + 1
	if result >= 52 {
		day4 = date2j(year+1, 1, 4)
		day0 = j2day(day4 - 1)
		if dayn >= day4-day0 {
			result = (dayn-(day4-day0))/7 + 1
		}
	}
	return result
}

// date2isoyear is date2isoyear.
func date2isoyear(year, mon, mday int32) int32 {
	dayn := date2j(year, mon, mday)
	day4 := date2j(year, 1, 4)
	day0 := j2day(day4 - 1)
	if dayn < day4-day0 {
		day4 = date2j(year-1, 1, 4)
		day0 = j2day(day4 - 1)
		year--
	}
	result := (dayn-(day4-day0))/7 + 1
	if result >= 52 {
		day4 = date2j(year+1, 1, 4)
		day0 = j2day(day4 - 1)
		if dayn >= day4-day0 {
			year++
		}
	}
	return year
}

// pgTimestampTrunc is timestamp_trunc.
func pgTimestampTrunc(units string, ts pgTimestamp) (any, error) {
	if pgTimestampNotFinite(int64(ts)) {
		return ts, nil
	}
	low := downcaseTruncateIdentifier(units)
	typ, val := decodeUnits(low)
	if typ != tokUnits {
		return nil, pgErrorf("timestamp units \"%s\" not recognized", low)
	}
	tm, fsec, _, _, ok := timestamp2tm(int64(ts), false, pgSessionZone)
	if !ok {
		return nil, pgErrorf("timestamp out of range")
	}
	if _, ok := truncTm(val, &tm, &fsec); !ok {
		return nil, pgErrorf("timestamp units \"%s\" not supported", low)
	}
	r, ok := tm2timestamp(tm, fsec, nil)
	if !ok {
		return nil, pgErrorf("timestamp out of range")
	}
	return pgTimestamp(r), nil
}

// pgTimestamptzTrunc is timestamptz_trunc_internal.
func pgTimestamptzTrunc(units string, ts pgTimestamptz, z pgZone) (any, error) {
	low := downcaseTruncateIdentifier(units)
	typ, val := decodeUnits(low)
	if typ != tokUnits {
		return nil, pgErrorf("timestamp with time zone units \"%s\" not recognized", low)
	}
	tm, fsec, tz, _, ok := timestamp2tm(int64(ts), true, z)
	if !ok {
		return nil, pgErrorf("timestamp out of range")
	}
	redotz, ok := truncTm(val, &tm, &fsec)
	if !ok {
		return nil, pgErrorf("timestamp with time zone units \"%s\" not supported", low)
	}
	if redotz {
		tz, _ = determineTimeZoneOffset(&tm, z)
	}
	r, ok := tm2timestamp(tm, fsec, &tz)
	if !ok {
		return nil, pgErrorf("timestamp out of range")
	}
	return pgTimestamptz(r), nil
}

// pgIntervalTrunc is interval_trunc.
func pgIntervalTrunc(units string, iv pgInterval) (any, error) {
	low := downcaseTruncateIdentifier(units)
	typ, val := decodeUnits(low)
	if typ != tokUnits {
		return nil, pgErrorf("interval units \"%s\" not recognized", low)
	}
	tm, fsec, err := interval2tm(iv)
	if err != nil {
		return nil, err
	}
	steps := []int32{dtkMillennium, dtkCentury, dtkDecade, dtkYear, dtkQuarter, dtkMonth, dtkDay, dtkHour, dtkMinute, dtkSecond}
	start := -1
	for i, s := range steps {
		if s == val {
			start = i
		}
	}
	switch {
	case start >= 0:
		for _, s := range steps[start:] {
			switch s {
			case dtkMillennium:
				tm.year = (tm.year / 1000) * 1000
			case dtkCentury:
				tm.year = (tm.year / 100) * 100
			case dtkDecade:
				tm.year = (tm.year / 10) * 10
			case dtkYear:
				tm.mon = 0
			case dtkQuarter:
				tm.mon = 3 * (tm.mon / 3)
			case dtkMonth:
				tm.mday = 0
			case dtkDay:
				tm.hour = 0
			case dtkHour:
				tm.min = 0
			case dtkMinute:
				tm.sec = 0
			case dtkSecond:
				fsec = 0
			}
		}
	case val == dtkMillisec:
		fsec = (fsec / 1000) * 1000
	case val == dtkMicrosec:
	case val == dtkWeek:
		return nil, pgErrorf("interval units \"%s\" not supported because months usually have fractional weeks", low)
	default:
		return nil, pgErrorf("interval units \"%s\" not supported", low)
	}
	r, ok := tm2interval(tm, fsec)
	if !ok {
		return nil, pgErrorf("interval out of range")
	}
	return r, nil
}

// nonFiniteTimestampPart is NonFiniteTimestampTzPart; ok false is NULL.
func nonFiniteTimestampPart(typ int, unit int32, low string, negative, isTz bool) (float64, bool, error) {
	kind := "timestamp"
	if isTz {
		kind = "timestamp with time zone"
	}
	if typ != tokUnits && typ != tokReserv {
		return 0, false, pgErrorf("%s units \"%s\" not recognized", kind, low)
	}
	switch unit {
	case dtkMicrosec, dtkMillisec, dtkSecond, dtkMinute, dtkHour, dtkDay, dtkMonth, dtkQuarter, dtkWeek,
		dtkDow, dtkISODow, dtkDoy, dtkTZ, dtkTZMinute, dtkTZHour:
		return 0, false, nil
	case dtkYear, dtkDecade, dtkCentury, dtkMillennium, dtkJulian, dtkISOYear, dtkEpoch:
		if negative {
			return math.Inf(-1), true, nil
		}
		return math.Inf(1), true, nil
	}
	return 0, false, pgErrorf("%s units \"%s\" not supported", kind, low)
}

// timestampPart is timestamp_part_common and timestamptz_part_common, returning double precision.
func timestampPart(units string, ts int64, isTz bool) (any, error) {
	kind := "timestamp"
	if isTz {
		kind = "timestamp with time zone"
	}
	low := downcaseTruncateIdentifier(units)
	typ, val := decodeUnits(low)
	if typ == tokUnknown {
		typ, val = decodeSpecial(low)
	}
	if pgTimestampNotFinite(ts) {
		r, ok, err := nonFiniteTimestampPart(typ, val, low, ts == pgDTNoBegin, isTz)
		if err != nil || !ok {
			return nil, err
		}
		return pgFloat8(r), nil
	}
	notSupported := pgErrorf("%s units \"%s\" not supported", kind, low)
	var intresult int64
	switch typ {
	case tokUnits:
		tm, fsec, tz, _, ok := timestamp2tm(ts, isTz, pgSessionZone)
		if !ok {
			return nil, pgErrorf("timestamp out of range")
		}
		switch val {
		case dtkMicrosec:
			intresult = int64(tm.sec)*1000000 + fsec
		case dtkMillisec:
			return pgFloat8(float64(tm.sec)*1000.0 + float64(fsec)/1000.0), nil
		case dtkSecond:
			return pgFloat8(float64(tm.sec) + float64(fsec)/1000000.0), nil
		case dtkMinute:
			intresult = int64(tm.min)
		case dtkHour:
			intresult = int64(tm.hour)
		case dtkDay:
			intresult = int64(tm.mday)
		case dtkMonth:
			intresult = int64(tm.mon)
		case dtkQuarter:
			intresult = int64((tm.mon-1)/3 + 1)
		case dtkWeek:
			intresult = int64(date2isoweek(tm.year, tm.mon, tm.mday))
		case dtkYear:
			if tm.year > 0 {
				intresult = int64(tm.year)
			} else {
				intresult = int64(tm.year) - 1
			}
		case dtkDecade:
			if tm.year >= 0 {
				intresult = int64(tm.year / 10)
			} else {
				intresult = int64(-((8 - (tm.year - 1)) / 10))
			}
		case dtkCentury:
			if tm.year > 0 {
				intresult = int64((tm.year + 99) / 100)
			} else {
				intresult = int64(-((99 - (tm.year - 1)) / 100))
			}
		case dtkMillennium:
			if tm.year > 0 {
				intresult = int64((tm.year + 999) / 1000)
			} else {
				intresult = int64(-((999 - (tm.year - 1)) / 1000))
			}
		case dtkJulian:
			secs := float64((((tm.hour*pgMinsPerHour)+tm.min)*pgSecsPerMinute)+tm.sec) + float64(fsec)/1000000.0
			return pgFloat8(float64(date2j(tm.year, tm.mon, tm.mday)) + secs/float64(pgSecsPerDay)), nil
		case dtkISOYear:
			intresult = int64(date2isoyear(tm.year, tm.mon, tm.mday))
			if intresult <= 0 {
				intresult--
			}
		case dtkDow, dtkISODow:
			intresult = int64(j2day(date2j(tm.year, tm.mon, tm.mday)))
			if val == dtkISODow && intresult == 0 {
				intresult = 7
			}
		case dtkDoy:
			intresult = int64(date2j(tm.year, tm.mon, tm.mday) - date2j(tm.year, 1, 1) + 1)
		case dtkTZ:
			if !isTz {
				return nil, notSupported
			}
			intresult = int64(-tz)
		case dtkTZMinute:
			if !isTz {
				return nil, notSupported
			}
			intresult = int64((-tz / pgSecsPerMinute) % pgMinsPerHour)
		case dtkTZHour:
			if !isTz {
				return nil, notSupported
			}
			intresult = int64(-tz / pgSecsPerHour)
		default:
			return nil, notSupported
		}
	case tokReserv:
		if val != dtkEpoch {
			return nil, notSupported
		}
		epoch := pgEpochTimestamp()
		if ts < math.MaxInt64+epoch {
			return pgFloat8(float64(ts-epoch) / 1000000.0), nil
		}
		return pgFloat8((float64(ts) - float64(epoch)) / 1000000.0), nil
	default:
		return nil, pgErrorf("%s units \"%s\" not recognized", kind, low)
	}
	return pgFloat8(float64(intresult)), nil
}

// intervalPart is interval_part_common, returning double precision.
func intervalPart(units string, iv pgInterval) (any, error) {
	low := downcaseTruncateIdentifier(units)
	typ, val := decodeUnits(low)
	if typ == tokUnknown {
		typ, val = decodeSpecial(low)
	}
	switch {
	case typ == tokUnits:
		tm, fsec, err := interval2tm(iv)
		if err != nil {
			return nil, err
		}
		var r int64
		switch val {
		case dtkMicrosec:
			r = int64(tm.sec)*1000000 + fsec
		case dtkMillisec:
			return pgFloat8(float64(tm.sec)*1000.0 + float64(fsec)/1000.0), nil
		case dtkSecond:
			return pgFloat8(float64(tm.sec) + float64(fsec)/1000000.0), nil
		case dtkMinute:
			r = int64(tm.min)
		case dtkHour:
			r = int64(tm.hour)
		case dtkDay:
			r = int64(tm.mday)
		case dtkMonth:
			r = int64(tm.mon)
		case dtkQuarter:
			r = int64(tm.mon/3 + 1)
		case dtkYear:
			r = int64(tm.year)
		case dtkDecade:
			r = int64(tm.year / 10)
		case dtkCentury:
			r = int64(tm.year / 100)
		case dtkMillennium:
			r = int64(tm.year / 1000)
		default:
			return nil, pgErrorf("interval units \"%s\" not supported", low)
		}
		return pgFloat8(float64(r)), nil
	case typ == tokReserv && val == dtkEpoch:
		result := float64(iv.time) / 1000000.0
		result += (float64(pgDaysPerYear) * pgSecsPerDay) * float64(iv.month/pgMonthsPerYear)
		result += (float64(pgDaysPerMonth) * pgSecsPerDay) * float64(iv.month%pgMonthsPerYear)
		result += float64(pgSecsPerDay) * float64(iv.day)
		return pgFloat8(result), nil
	}
	return nil, pgErrorf("interval units \"%s\" not recognized", low)
}

// pgZoneArg is the zone a function's text argument names, as timestamptz_trunc_zone reads it: an
// abbreviation first, then a zone name.
func pgZoneArg(name string) (pgZone, error) {
	if len(name) > 255 {
		name = name[:255]
	}
	low := downcaseTruncateIdentifier(name)
	typ, val, z, _ := decodeTimezoneAbbrev(low)
	switch typ {
	case tokTZ, tokDTZ:
		return pgTzsetOffset(int64(val)), nil
	case tokDynTZ:
		return z, nil
	}
	if z, ok := pgTzset(name); ok {
		return z, nil
	}
	return pgZone{}, pgErrorf("time zone \"%s\" not recognized", name)
}

func init() {
	registerPgEnv("now()", func(e pgEnv, _ []any) (any, error) { return e.decode().now, nil })
	registerPg("to_timestamp(float8)", func(a []any) (any, error) { return pgFloat8Timestamptz(f8(a[0])) })
	registerPg("date_trunc(text,timestamp)", func(a []any) (any, error) {
		return pgTimestampTrunc(a[0].(string), a[1].(pgTimestamp))
	})
	registerPg("date_trunc(text,timestamptz)", func(a []any) (any, error) {
		ts := a[1].(pgTimestamptz)
		if pgTimestampNotFinite(int64(ts)) {
			return ts, nil
		}
		return pgTimestamptzTrunc(a[0].(string), ts, pgSessionZone)
	})
	registerPg("date_trunc(text,timestamptz,text)", func(a []any) (any, error) {
		ts := a[1].(pgTimestamptz)
		if pgTimestampNotFinite(int64(ts)) {
			return ts, nil
		}
		z, err := pgZoneArg(a[2].(string))
		if err != nil {
			return nil, err
		}
		return pgTimestamptzTrunc(a[0].(string), ts, z)
	})
	registerPg("date_trunc(text,interval)", func(a []any) (any, error) {
		return pgIntervalTrunc(a[0].(string), a[1].(pgInterval))
	})
	registerPg("date_part(text,timestamp)", func(a []any) (any, error) {
		return timestampPart(a[0].(string), int64(a[1].(pgTimestamp)), false)
	})
	registerPg("date_part(text,timestamptz)", func(a []any) (any, error) {
		return timestampPart(a[0].(string), int64(a[1].(pgTimestamptz)), true)
	})
	registerPg("date_part(text,interval)", func(a []any) (any, error) {
		return intervalPart(a[0].(string), a[1].(pgInterval))
	})
	// date_part(text, date) is an SQL function over $2::timestamp.
	registerPg("date_part(text,date)", func(a []any) (any, error) {
		ts, err := pgDateToTimestamp(a[1].(pgDate))
		if err != nil {
			return nil, err
		}
		return timestampPart(a[0].(string), int64(ts), false)
	})
}
