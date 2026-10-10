package sqlfn

import (
	"math"
	"strings"
)

// The date and time types' input functions (timestamp_in, timestamptz_in, date_in, interval_in) and
// casts, over datetime.c's parsing.

// mulAdd is a*b+c as Postgres's own build computes it: fused on aarch64, where GCC contracts it.
func (e decodeEnv) mulAdd(a, b, c float64) float64 {
	if e.contract {
		return math.FMA(a, b, c)
	}
	return float64(a*b) + c
}

// adjustFractSeconds is AdjustFractSeconds.
func adjustFractSeconds(frac float64, tm *pgTm, fsec *int64, scale int) {
	if frac == 0 {
		return
	}
	frac *= float64(scale)
	sec := int32(frac)
	tm.sec += sec
	frac -= float64(sec)
	*fsec = int64(float64(*fsec) + math.RoundToEven(frac*1000000))
}

// adjustFractDays is AdjustFractDays.
func adjustFractDays(frac float64, tm *pgTm, fsec *int64, scale int) {
	if frac == 0 {
		return
	}
	frac *= float64(scale)
	extra := int32(frac)
	tm.mday += extra
	frac -= float64(extra)
	adjustFractSeconds(frac, tm, fsec, pgSecsPerDay)
}

// decodeInterval is DecodeInterval, in IntervalStyle postgres.
func decodeInterval(env decodeEnv, field []string, ftype []int, rng int, tm *pgTm, fsec *int64) int {
	isBefore := false
	fmask, tmask := 0, 0
	typ := tokIgnore
	*tm = pgTm{}
	*fsec = 0
	for i := len(field) - 1; i >= 0; i-- {
		f := field[i]
		switch ftype[i] {
		case dtkTime:
			var err int
			tmask, err = decodeTime(f, rng, tm, fsec)
			if err != 0 {
				return err
			}
			typ = dtkDay
		case dtkTZ, dtkDate, dtkNumber:
			if ftype[i] == dtkTZ && strings.IndexByte(f[1:], ':') >= 0 {
				// A failed DecodeTime leaves what it set, as in C.
				if m, err := decodeTime(f[1:], rng, tm, fsec); err == 0 {
					tmask = m
					if f[0] == '-' {
						tm.hour, tm.min, tm.sec = -tm.hour, -tm.min, -tm.sec
						*fsec = -*fsec
					}
					typ = dtkDay
					break
				}
			}
			if typ == tokIgnore {
				switch rng {
				case 1 << tokYear:
					typ = dtkYear
				case 1 << tokMonth, 1<<tokYear | 1<<tokMonth:
					typ = dtkMonth
				case 1 << tokDay:
					typ = dtkDay
				case 1 << tokHour, 1<<tokDay | 1<<tokHour:
					typ = dtkHour
				case 1 << tokMinute, 1<<tokHour | 1<<tokMinute, 1<<tokDay | 1<<tokHour | 1<<tokMinute:
					typ = dtkMinute
				default:
					typ = dtkSecond
				}
			}
			val, cp, erange := cStrtoint(f, 0)
			if erange {
				return dtErrFieldOverflow
			}
			var fval float64
			switch cAt(f, cp) {
			case '-':
				val2, cp2, erange := cStrtoint(f, cp+1)
				if erange || val2 < 0 || val2 >= pgMonthsPerYear {
					return dtErrFieldOverflow
				}
				if cp2 != len(f) {
					return dtErrBadFormat
				}
				typ = dtkMonth
				if f[0] == '-' {
					val2 = -val2
				}
				if t := float64(val)*pgMonthsPerYear + float64(val2); t > math.MaxInt32 || t < math.MinInt32 {
					return dtErrFieldOverflow
				}
				val = val*pgMonthsPerYear + val2
				fval = 0
			case '.':
				v, n, erange := cStrtod(f, cp)
				if n != len(f) || erange {
					return dtErrBadFormat
				}
				fval = v
				if f[0] == '-' {
					fval = -fval
				}
			case 0:
				if cp != len(f) {
					return dtErrBadFormat
				}
				fval = 0
			default:
				return dtErrBadFormat
			}
			tmask = 0
			switch typ {
			case dtkMicrosec:
				*fsec = int64(float64(*fsec) + math.RoundToEven(float64(val)+fval))
				tmask = dtkM(tokMicrosecond)
			case dtkMillisec:
				tm.sec += val / 1000
				val -= (val / 1000) * 1000
				*fsec = int64(float64(*fsec) + math.RoundToEven((float64(val)+fval)*1000))
				tmask = dtkM(tokMillisecond)
			case dtkSecond:
				tm.sec += val
				*fsec = int64(float64(*fsec) + math.RoundToEven(fval*1000000))
				if fval == 0 {
					tmask = dtkM(tokSecond)
				} else {
					tmask = dtkAllSecsM
				}
			case dtkMinute:
				tm.min += val
				adjustFractSeconds(fval, tm, fsec, pgSecsPerMinute)
				tmask = dtkM(tokMinute)
			case dtkHour:
				tm.hour += val
				adjustFractSeconds(fval, tm, fsec, pgSecsPerHour)
				tmask = dtkM(tokHour)
				typ = dtkDay
			case dtkDay:
				tm.mday += val
				adjustFractSeconds(fval, tm, fsec, pgSecsPerDay)
				tmask = dtkM(tokDay)
			case dtkWeek:
				tm.mday += val * 7
				adjustFractDays(fval, tm, fsec, 7)
				tmask = dtkM(tokWeek)
			case dtkMonth:
				tm.mon += val
				adjustFractDays(fval, tm, fsec, pgDaysPerMonth)
				tmask = dtkM(tokMonth)
			case dtkYear:
				tm.year += val
				if fval != 0 {
					tm.mon = int32(env.mulAdd(fval, pgMonthsPerYear, float64(tm.mon)))
				}
				tmask = dtkM(tokYear)
			case dtkDecade, dtkCentury, dtkMillennium:
				scale, mask := int32(10), dtkM(tokDecade)
				if typ == dtkCentury {
					scale, mask = 100, dtkM(tokCentury)
				} else if typ == dtkMillennium {
					scale, mask = 1000, dtkM(tokMillennium)
				}
				tm.year += val * scale
				if fval != 0 {
					// tm_mon += fval * MONTHS_PER_YEAR * scale: the last product feeds the addition.
					tm.mon = int32(env.mulAdd(float64(fval*pgMonthsPerYear), float64(scale), float64(tm.mon)))
				}
				tmask = mask
			default:
				return dtErrBadFormat
			}
		case dtkString, dtkSpecial:
			t, val := decodeUnits(f)
			if t == tokIgnore {
				typ = t
				continue
			}
			tmask = 0
			switch t {
			case tokUnits:
				typ = int(val)
			case tokAgo:
				isBefore = true
				typ = int(val)
			case tokReserv:
				tmask = dtkDateM | dtkTimeM
				return pgDTErrUnexpectedDtype
			default:
				return dtErrBadFormat
			}
		default:
			return dtErrBadFormat
		}
		if tmask&fmask != 0 {
			return dtErrBadFormat
		}
		fmask |= tmask
	}
	if fmask == 0 {
		return dtErrBadFormat
	}
	if *fsec != 0 {
		sec := *fsec / pgUsecsPerSec
		*fsec -= sec * pgUsecsPerSec
		tm.sec += int32(sec)
	}
	if isBefore {
		*fsec = -*fsec
		tm.sec, tm.min, tm.hour = -tm.sec, -tm.min, -tm.hour
		tm.mday, tm.mon, tm.year = -tm.mday, -tm.mon, -tm.year
	}
	return 0
}

// pgDTErrUnexpectedDtype is an input whose decoding gives a type the input function cannot take.
const pgDTErrUnexpectedDtype = -101

// parseISO8601Number is ParseISO8601Number.
func parseISO8601Number(s string, i int) (ipart int32, fpart float64, end int, err int) {
	c := cAt(s, i)
	if !(cIsDigit(c) || c == '-' || c == '.') {
		return 0, 0, i, dtErrBadFormat
	}
	val, n, erange := cStrtod(s, i)
	if n == i || erange {
		return 0, 0, i, dtErrBadFormat
	}
	if val < math.MinInt32 || val > math.MaxInt32 {
		return 0, 0, n, dtErrFieldOverflow
	}
	if val >= 0 {
		ipart = int32(math.Floor(val))
	} else {
		ipart = int32(-math.Floor(-val))
	}
	return ipart, val - float64(ipart), n, 0
}

func iso8601IntegerWidth(s string, i int) int {
	if cAt(s, i) == '-' {
		i++
	}
	n := 0
	for cIsDigit(cAt(s, i+n)) {
		n++
	}
	return n
}

// decodeISO8601Interval is DecodeISO8601Interval.
func decodeISO8601Interval(env decodeEnv, s string, tm *pgTm, fsec *int64) int {
	datepart, havefield := true, false
	*tm = pgTm{}
	*fsec = 0
	if len(s) < 2 || s[0] != 'P' {
		return dtErrBadFormat
	}
	i := 1
	for i < len(s) {
		if s[i] == 'T' {
			datepart, havefield = false, false
			i++
			continue
		}
		fieldstart := i
		val, fval, n, err := parseISO8601Number(s, i)
		if err != 0 {
			return err
		}
		i = n
		unit := cAt(s, i)
		i++
		if datepart {
			switch unit {
			case 'Y':
				tm.year += val
				tm.mon = int32(env.mulAdd(fval, pgMonthsPerYear, float64(tm.mon)))
			case 'M':
				tm.mon += val
				adjustFractDays(fval, tm, fsec, pgDaysPerMonth)
			case 'W':
				tm.mday += val * 7
				adjustFractDays(fval, tm, fsec, 7)
			case 'D':
				tm.mday += val
				adjustFractSeconds(fval, tm, fsec, pgSecsPerDay)
			case 'T', 0, '-':
				if unit != '-' && iso8601IntegerWidth(s, fieldstart) == 8 && !havefield {
					tm.year += val / 10000
					tm.mon += (val / 100) % 100
					tm.mday += val % 100
					adjustFractSeconds(fval, tm, fsec, pgSecsPerDay)
					if unit == 0 {
						return 0
					}
					datepart, havefield = false, false
					continue
				}
				if havefield {
					return dtErrBadFormat
				}
				tm.year += val
				tm.mon = int32(env.mulAdd(fval, pgMonthsPerYear, float64(tm.mon)))
				if unit == 0 {
					return 0
				}
				if unit == 'T' {
					datepart, havefield = false, false
					continue
				}
				val, fval, n, err = parseISO8601Number(s, i)
				if err != 0 {
					return err
				}
				i = n
				tm.mon += val
				adjustFractDays(fval, tm, fsec, pgDaysPerMonth)
				switch cAt(s, i) {
				case 0:
					return 0
				case 'T':
					datepart, havefield = false, false
					i++
					continue
				case '-':
				default:
					return dtErrBadFormat
				}
				i++
				val, fval, n, err = parseISO8601Number(s, i)
				if err != 0 {
					return err
				}
				i = n
				tm.mday += val
				adjustFractSeconds(fval, tm, fsec, pgSecsPerDay)
				switch cAt(s, i) {
				case 0:
					return 0
				case 'T':
					datepart, havefield = false, false
					i++
					continue
				}
				return dtErrBadFormat
			default:
				return dtErrBadFormat
			}
		} else {
			switch unit {
			case 'H':
				tm.hour += val
				adjustFractSeconds(fval, tm, fsec, pgSecsPerHour)
			case 'M':
				tm.min += val
				adjustFractSeconds(fval, tm, fsec, pgSecsPerMinute)
			case 'S':
				tm.sec += val
				adjustFractSeconds(fval, tm, fsec, 1)
			case 0, ':':
				if unit == 0 && iso8601IntegerWidth(s, fieldstart) == 6 && !havefield {
					tm.hour += val / 10000
					tm.min += (val / 100) % 100
					tm.sec += val % 100
					adjustFractSeconds(fval, tm, fsec, 1)
					return 0
				}
				if havefield {
					return dtErrBadFormat
				}
				tm.hour += val
				adjustFractSeconds(fval, tm, fsec, pgSecsPerHour)
				if unit == 0 {
					return 0
				}
				val, fval, n, err = parseISO8601Number(s, i)
				if err != 0 {
					return err
				}
				i = n
				tm.min += val
				adjustFractSeconds(fval, tm, fsec, pgSecsPerMinute)
				if cAt(s, i) == 0 {
					return 0
				}
				if cAt(s, i) != ':' {
					return dtErrBadFormat
				}
				i++
				val, fval, n, err = parseISO8601Number(s, i)
				if err != 0 {
					return err
				}
				i = n
				tm.sec += val
				adjustFractSeconds(fval, tm, fsec, 1)
				if cAt(s, i) == 0 {
					return 0
				}
				return dtErrBadFormat
			default:
				return dtErrBadFormat
			}
		}
		havefield = true
	}
	return 0
}

// dateTimeParseError is DateTimeParseError.
func dateTimeParseError(dterr int, str, datatype string) error {
	switch dterr {
	case dtErrFieldOverflow, dtErrMDFieldOverflow:
		return pgErrorf("date/time field value out of range: \"%s\"", str)
	case dtErrIntervalOverflow:
		return pgErrorf("interval field value out of range: \"%s\"", str)
	case dtErrTZDispOverflow:
		return pgErrorf("time zone displacement out of range: \"%s\"", str)
	}
	return pgErrorf("invalid input syntax for type %s: \"%s\"", datatype, str)
}

// decodeTimestampText is what timestamp_in, timestamptz_in and date_in share: the parse.
func decodeTimestampText(env decodeEnv, str, datatype string, buflen int) (pgTm, int64, int32, int, error) {
	var tm pgTm
	var fsec int64
	var tz int32
	field, ftype, dterr := parseDateTime(str, buflen)
	dtype := 0
	if dterr == 0 {
		dtype, dterr = decodeDateTime(env, field, ftype, &tm, &fsec, &tz)
	}
	if dterr == pgDTErrTZNotRecognized {
		return tm, 0, 0, 0, pgErrorf("time zone \"%s\" not recognized", tm.zone)
	}
	if dterr != 0 {
		return tm, 0, 0, 0, dateTimeParseError(dterr, str, datatype)
	}
	return tm, fsec, tz, dtype, nil
}

// pgTimestamptzIn is timestamptz_in.
func pgTimestamptzIn(env decodeEnv, str string) (any, error) {
	tm, fsec, tz, dtype, err := decodeTimestampText(env, str, "timestamp with time zone", pgMaxDateLen+pgMaxDateFields)
	if err != nil {
		return nil, err
	}
	switch dtype {
	case dtkDate:
		r, ok := tm2timestamp(tm, fsec, &tz)
		if !ok {
			return nil, pgErrorf("timestamp out of range: \"%s\"", str)
		}
		return pgTimestamptz(r), nil
	case dtkEpoch:
		return pgTimestamptz(pgEpochTimestamp()), nil
	case dtkLate:
		return pgTimestamptz(pgDTNoEnd), nil
	case dtkEarly:
		return pgTimestamptz(pgDTNoBegin), nil
	}
	return nil, pgErrorf("unexpected dtype %d while parsing timestamptz \"%s\"", dtype, str)
}

// pgTimestampIn is timestamp_in: a zone in the text is read and ignored.
func pgTimestampIn(env decodeEnv, str string) (any, error) {
	tm, fsec, _, dtype, err := decodeTimestampText(env, str, "timestamp", pgMaxDateLen+pgMaxDateFields)
	if err != nil {
		return nil, err
	}
	switch dtype {
	case dtkDate:
		r, ok := tm2timestamp(tm, fsec, nil)
		if !ok {
			return nil, pgErrorf("timestamp out of range: \"%s\"", str)
		}
		return pgTimestamp(r), nil
	case dtkEpoch:
		return pgTimestamp(pgEpochTimestamp()), nil
	case dtkLate:
		return pgTimestamp(pgDTNoEnd), nil
	case dtkEarly:
		return pgTimestamp(pgDTNoBegin), nil
	}
	return nil, pgErrorf("unexpected dtype %d while parsing timestamp \"%s\"", dtype, str)
}

// pgDateIn is date_in.
func pgDateIn(env decodeEnv, str string) (any, error) {
	tm, _, _, dtype, err := decodeTimestampText(env, str, "date", pgMaxDateLen+1)
	if err != nil {
		return nil, err
	}
	switch dtype {
	case dtkDate:
	case dtkEpoch:
		tm = pgTm{year: 1970, mon: 1, mday: 1}
	case dtkLate:
		return pgDate(math.MaxInt32), nil
	case dtkEarly:
		return pgDate(math.MinInt32), nil
	default:
		return nil, dateTimeParseError(dtErrBadFormat, str, "date")
	}
	if !pgIsValidJulian(tm.year, tm.mon, tm.mday) {
		return nil, pgErrorf("date out of range: \"%s\"", str)
	}
	d := date2j(tm.year, tm.mon, tm.mday) - pgPostgresEpochJDate
	if !pgIsValidDate(d) {
		return nil, pgErrorf("date out of range: \"%s\"", str)
	}
	return pgDate(d), nil
}

// pgIntervalIn is interval_in.
func pgIntervalIn(env decodeEnv, str string) (any, error) {
	var tm pgTm
	var fsec int64
	field, ftype, dterr := parseDateTime(str, 256)
	if dterr == 0 {
		dterr = decodeInterval(env, field, ftype, pgIntervalFullRange, &tm, &fsec)
	}
	if dterr == dtErrBadFormat {
		dterr = decodeISO8601Interval(env, str, &tm, &fsec)
	}
	if dterr == pgDTErrUnexpectedDtype {
		return nil, pgErrorf("unexpected dtype while parsing interval \"%s\"", str)
	}
	if dterr != 0 {
		if dterr == dtErrFieldOverflow {
			dterr = dtErrIntervalOverflow
		}
		return nil, dateTimeParseError(dterr, str, "interval")
	}
	r, ok := tm2interval(tm, fsec)
	if !ok {
		return nil, pgErrorf("interval out of range")
	}
	return r, nil
}

// pgDateToTimestamp is date2timestamp.
func pgDateToTimestamp(d pgDate) (pgTimestamp, error) {
	switch int32(d) {
	case math.MinInt32:
		return pgTimestamp(pgDTNoBegin), nil
	case math.MaxInt32:
		return pgTimestamp(pgDTNoEnd), nil
	}
	if int32(d) >= pgTimestampEndJulian-pgPostgresEpochJDate {
		return 0, pgErrorf("date out of range for timestamp")
	}
	return pgTimestamp(int64(d) * pgUsecsPerDay), nil
}

// pgDateToTimestamptz is date2timestamptz.
func pgDateToTimestamptz(d pgDate) (pgTimestamptz, error) {
	switch int32(d) {
	case math.MinInt32:
		return pgTimestamptz(pgDTNoBegin), nil
	case math.MaxInt32:
		return pgTimestamptz(pgDTNoEnd), nil
	}
	if int32(d) >= pgTimestampEndJulian-pgPostgresEpochJDate {
		return 0, pgErrorf("date out of range for timestamp")
	}
	var tm pgTm
	tm.year, tm.mon, tm.mday = j2date(int32(d) + pgPostgresEpochJDate)
	tz, _ := determineTimeZoneOffset(&tm, pgSessionZone)
	r := int64(d)*pgUsecsPerDay + int64(tz)*pgUsecsPerSec
	if !pgIsValidTimestamp(r) {
		return 0, pgErrorf("date out of range for timestamp")
	}
	return pgTimestamptz(r), nil
}

// pgTimestampToTimestamptz is timestamp2timestamptz.
func pgTimestampToTimestamptz(t pgTimestamp) (pgTimestamptz, error) {
	if pgTimestampNotFinite(int64(t)) {
		return pgTimestamptz(t), nil
	}
	tm, _, _, _, ok := timestamp2tm(int64(t), false, pgSessionZone)
	if ok {
		tz, _ := determineTimeZoneOffset(&tm, pgSessionZone)
		r := int64(t) + int64(tz)*pgUsecsPerSec
		if pgIsValidTimestamp(r) {
			return pgTimestamptz(r), nil
		}
	}
	return 0, pgErrorf("timestamp out of range")
}

// pgTimestamptzToTimestamp is timestamptz2timestamp.
func pgTimestamptzToTimestamp(t pgTimestamptz) (pgTimestamp, error) {
	if pgTimestampNotFinite(int64(t)) {
		return pgTimestamp(t), nil
	}
	tm, fsec, _, _, ok := timestamp2tm(int64(t), true, pgSessionZone)
	if !ok {
		return 0, pgErrorf("timestamp out of range")
	}
	r, ok := tm2timestamp(tm, fsec, nil)
	if !ok {
		return 0, pgErrorf("timestamp out of range")
	}
	return pgTimestamp(r), nil
}

// pgTimestampDate is timestamp_date and timestamptz_date.
func pgTimestampDate(ts int64, withZone bool) (any, error) {
	switch ts {
	case pgDTNoBegin:
		return pgDate(math.MinInt32), nil
	case pgDTNoEnd:
		return pgDate(math.MaxInt32), nil
	}
	tm, _, _, _, ok := timestamp2tm(ts, withZone, pgSessionZone)
	if !ok {
		return nil, pgErrorf("timestamp out of range")
	}
	return pgDate(date2j(tm.year, tm.mon, tm.mday) - pgPostgresEpochJDate), nil
}
