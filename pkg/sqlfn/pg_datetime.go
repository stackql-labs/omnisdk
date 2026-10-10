package sqlfn

import (
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Postgres's date and time types (timestamp.c, date.c, datetime.c), as stackql's Postgres runs
// them: session TimeZone Etc/UTC, DateStyle ISO, MDY, IntervalStyle postgres.

type (
	pgTimestamp   int64 // microseconds from 2000-01-01 00:00, no zone
	pgTimestamptz int64 // microseconds from 2000-01-01 00:00 UTC
	pgDate        int32 // days from 2000-01-01
)

// pgInterval is an interval: months, days and microseconds, kept apart as Postgres keeps them.
type pgInterval struct {
	time  int64
	day   int32
	month int32
}

const (
	pgUsecsPerDay    int64 = 86400000000
	pgUsecsPerHour   int64 = 3600000000
	pgUsecsPerMinute int64 = 60000000
	pgUsecsPerSec    int64 = 1000000
	pgSecsPerDay           = 86400
	pgSecsPerHour          = 3600
	pgSecsPerMinute        = 60
	pgMinsPerHour          = 60
	pgHoursPerDay          = 24
	pgMonthsPerYear        = 12
	pgDaysPerMonth         = 30
	pgDaysPerYear          = 365.25

	pgPostgresEpochJDate = 2451545
	pgUnixEpochJDate     = 2440588
	pgDatetimeMinJulian  = 0
	pgDateEndJulian      = 2147483494
	pgTimestampEndJulian = 109203528

	pgMinTimestamp int64 = -211813488000000000
	pgEndTimestamp int64 = 9223371331200000000
	pgDTNoBegin    int64 = math.MinInt64
	pgDTNoEnd      int64 = math.MaxInt64

	pgJulianMinYear  = -4713
	pgJulianMinMonth = 11
	pgJulianMaxYear  = 5874898
	pgJulianMaxMonth = 6

	pgMaxTZDispHour         = 15
	pgMaxTimestampPrecision = 6
	pgMaxIntervalPrecision  = 6
)

func pgIsValidJulian(y, m, d int32) bool {
	return (y > pgJulianMinYear || (y == pgJulianMinYear && m >= pgJulianMinMonth)) &&
		(y < pgJulianMaxYear || (y == pgJulianMaxYear && m < pgJulianMaxMonth))
}

func pgIsValidTimestamp(t int64) bool { return pgMinTimestamp <= t && t < pgEndTimestamp }

func pgIsValidDate(d int32) bool {
	return pgDatetimeMinJulian-pgPostgresEpochJDate <= d && d < pgDateEndJulian-pgPostgresEpochJDate
}

func pgTimestampNotFinite(t int64) bool { return t == pgDTNoBegin || t == pgDTNoEnd }

// pgTm is struct pg_tm: the broken-down time, its year astronomical (1 BC is 0) and month from 1.
type pgTm struct {
	sec, min, hour, mday, mon, year, wday, yday, isdst int32
	gmtoff                                             int64
	zone                                               string
}

var pgDayTab = [2][13]int32{
	{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31, 0},
	{31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31, 0},
}

var pgMonthNames = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
var pgDayNames = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

func pgIsLeap(y int32) int {
	if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
		return 1
	}
	return 0
}

// date2j is the Julian day of a date, in C's wrapping int arithmetic.
func date2j(y, m, d int32) int32 {
	if m > 2 {
		m++
		y += 4800
	} else {
		m += 13
		y += 4799
	}
	century := y / 100
	julian := y*365 - 32167
	julian += y/4 - century + century/4
	julian += 7834*m/256 + d
	return julian
}

// j2date is date2j's inverse.
func j2date(jd int32) (year, month, day int32) {
	julian := uint32(jd)
	julian += 32044
	quad := julian / 146097
	extra := (julian-quad*146097)*4 + 3
	julian += 60 + quad*3 + extra/146097
	quad = julian / 1461
	julian -= quad * 1461
	y := int32(julian * 4 / 1461)
	if y != 0 {
		julian = (julian+305)%365 + 123
	} else {
		julian = (julian+306)%366 + 123
	}
	y += int32(quad * 4)
	year = y - 4800
	quad = julian * 2141 / 65536
	day = int32(julian - 7834*quad/256)
	month = int32((quad+10)%pgMonthsPerYear + 1)
	return
}

// j2day is the day of the week, Sunday 0.
func j2day(date int32) int32 {
	date++
	date %= 7
	if date < 0 {
		date += 7
	}
	return date
}

// dt2time splits microseconds of a day into hours, minutes, seconds and the fraction.
func dt2time(jd int64) (hour, min, sec int32, fsec int64) {
	t := jd
	hour = int32(t / pgUsecsPerHour)
	t -= int64(hour) * pgUsecsPerHour
	min = int32(t / pgUsecsPerMinute)
	t -= int64(min) * pgUsecsPerMinute
	sec = int32(t / pgUsecsPerSec)
	fsec = t - int64(sec)*pgUsecsPerSec
	return
}

func time2t(hour, min, sec int32, fsec int64) int64 {
	return ((((int64(hour)*pgMinsPerHour)+int64(min))*pgSecsPerMinute)+int64(sec))*pgUsecsPerSec + fsec
}

// pgZone is a pg_tz: a named zone, or a fixed offset.
type pgZone struct {
	loc *time.Location
}

// pgSessionZone is stackql's session TimeZone, Etc/UTC.
var pgSessionZone = pgZone{loc: time.UTC}

// pgLocalTime is pg_localtime: t, in Unix seconds, as wall time in z.
func (z pgZone) localTime(t int64) pgTm {
	lt := time.Unix(t, 0).In(z.loc)
	name, off := lt.Zone()
	tm := pgTm{year: int32(lt.Year()), mon: int32(lt.Month()), mday: int32(lt.Day()), hour: int32(lt.Hour()),
		min: int32(lt.Minute()), sec: int32(lt.Second()), gmtoff: int64(off), zone: name}
	if lt.IsDST() {
		tm.isdst = 1
	}
	return tm
}

// nextBoundary is pg_next_dst_boundary: the offset at t, the next transition after it and the
// offset there; found reports whether there is one.
func (z pgZone) nextBoundary(t int64) (beforeOff int64, beforeDST int32, boundary, afterOff int64, afterDST int32, found bool) {
	lt := time.Unix(t, 0).In(z.loc)
	_, off := lt.Zone()
	beforeOff = int64(off)
	if lt.IsDST() {
		beforeDST = 1
	}
	_, end := lt.ZoneBounds()
	if end.IsZero() {
		return beforeOff, beforeDST, 0, 0, 0, false
	}
	at := end.In(z.loc)
	_, aoff := at.Zone()
	afterOff = int64(aoff)
	if at.IsDST() {
		afterDST = 1
	}
	return beforeOff, beforeDST, end.Unix(), afterOff, afterDST, true
}

// interpretAbbrev is pg_interpret_timezone_abbrev: the offset abbr has in z at about t — in force
// then, else its latest use before, else its first after.
func (z pgZone) interpretAbbrev(abbr string, t int64) (gmtoff int64, isdst int32, ok bool) {
	check := func(at time.Time) bool {
		name, off := at.Zone()
		if !strings.EqualFold(name, abbr) {
			return false
		}
		gmtoff = int64(off)
		if at.IsDST() {
			isdst = 1
		}
		return true
	}
	at := time.Unix(t, 0).In(z.loc)
	for i := 0; i < 4000; i++ {
		if check(at) {
			return gmtoff, isdst, true
		}
		start, _ := at.ZoneBounds()
		if start.IsZero() {
			break
		}
		at = start.Add(-time.Second).In(z.loc)
	}
	at = time.Unix(t, 0).In(z.loc)
	for i := 0; i < 4000; i++ {
		_, end := at.ZoneBounds()
		if end.IsZero() {
			break
		}
		at = end.In(z.loc)
		if check(at) {
			return gmtoff, isdst, true
		}
	}
	return 0, 0, false
}

var (
	pgZoneMu    sync.Mutex
	pgZoneCache = map[string]*time.Location{}
)

// pgTzset is pg_tzset: the named zone, its name matched without regard to case, or false.
func pgTzset(name string) (pgZone, bool) {
	key := strings.ToLower(name)
	pgZoneMu.Lock()
	defer pgZoneMu.Unlock()
	if loc, ok := pgZoneCache[key]; ok {
		return pgZone{loc: loc}, loc != nil
	}
	var loc *time.Location
	if canon, ok := pgZoneNames[key]; ok {
		loc, _ = time.LoadLocation(canon)
	}
	pgZoneCache[key] = loc
	return pgZone{loc: loc}, loc != nil
}

// pgTzsetOffset is pg_tzset_offset: a zone fixed at gmtoff seconds east of UTC.
func pgTzsetOffset(gmtoff int64) pgZone {
	return pgZone{loc: time.FixedZone("", int(gmtoff))}
}

// timestamp2tm is timestamp2tm: dt broken down, in zone z where withZone, with the zone's offset
// (seconds west of UTC) and abbreviation.
func timestamp2tm(dt int64, withZone bool, z pgZone) (tm pgTm, fsec int64, tz int32, tzn string, ok bool) {
	t := dt
	date := t / pgUsecsPerDay
	if date != 0 {
		t -= date * pgUsecsPerDay
	}
	if t < 0 {
		t += pgUsecsPerDay
		date--
	}
	date += pgPostgresEpochJDate
	if date < 0 || date > math.MaxInt32 {
		return tm, 0, 0, "", false
	}
	tm.year, tm.mon, tm.mday = j2date(int32(date))
	tm.hour, tm.min, tm.sec, fsec = dt2time(t)
	if !withZone {
		tm.isdst = -1
		return tm, fsec, 0, "", true
	}
	utime := (dt-fsec)/pgUsecsPerSec + (pgPostgresEpochJDate-pgUnixEpochJDate)*pgSecsPerDay
	lt := z.localTime(utime)
	tm.year, tm.mon, tm.mday, tm.hour, tm.min, tm.sec = lt.year, lt.mon, lt.mday, lt.hour, lt.min, lt.sec
	tm.isdst, tm.gmtoff, tm.zone = lt.isdst, lt.gmtoff, lt.zone
	return tm, fsec, int32(-tm.gmtoff), tm.zone, true
}

// tm2timestamp is tm2timestamp: tm as a timestamp, shifted by tz (seconds west) where given.
func tm2timestamp(tm pgTm, fsec int64, tz *int32) (int64, bool) {
	if !pgIsValidJulian(tm.year, tm.mon, tm.mday) {
		return 0, false
	}
	date := int64(date2j(tm.year, tm.mon, tm.mday)) - pgPostgresEpochJDate
	t := time2t(tm.hour, tm.min, tm.sec, fsec)
	result := date*pgUsecsPerDay + t
	if (result-t)/pgUsecsPerDay != date {
		return 0, false
	}
	if (result < 0 && date > 0) || (result > 0 && date < -1) {
		return 0, false
	}
	if tz != nil {
		result -= int64(-*tz) * pgUsecsPerSec
	}
	if !pgIsValidTimestamp(result) {
		return 0, false
	}
	return result, true
}

// interval2tm is interval2tm; it fails where the hours overflow an int.
func interval2tm(span pgInterval) (tm pgTm, fsec int64, err error) {
	tm.year = span.month / pgMonthsPerYear
	tm.mon = span.month % pgMonthsPerYear
	tm.mday = span.day
	t := span.time
	tfrac := t / pgUsecsPerHour
	t -= tfrac * pgUsecsPerHour
	tm.hour = int32(tfrac)
	if (tm.hour < 0) != (tfrac < 0) {
		return tm, 0, pgErrorf("interval out of range")
	}
	tfrac = t / pgUsecsPerMinute
	t -= tfrac * pgUsecsPerMinute
	tm.min = int32(tfrac)
	tfrac = t / pgUsecsPerSec
	fsec = t - tfrac*pgUsecsPerSec
	tm.sec = int32(tfrac)
	return tm, fsec, nil
}

// tm2interval is tm2interval.
func tm2interval(tm pgTm, fsec int64) (pgInterval, bool) {
	total := float64(tm.year)*pgMonthsPerYear + float64(tm.mon)
	if total > math.MaxInt32 || total < math.MinInt32 {
		return pgInterval{}, false
	}
	return pgInterval{
		month: int32(total),
		day:   tm.mday,
		time:  ((((int64(tm.hour)*60)+int64(tm.min))*60)+int64(tm.sec))*pgUsecsPerSec + fsec,
	}, true
}

// pgEpochTimestamp is SetEpochTimestamp: 1970-01-01 00:00.
func pgEpochTimestamp() int64 {
	r, _ := tm2timestamp(pgTm{year: 1970, mon: 1, mday: 1}, 0, nil)
	return r
}

// determineTimeZoneOffset is DetermineTimeZoneOffsetInternal: the offset (seconds west) wall time
// tm has in z, with tm's isdst set, and the instant it names (Unix seconds).
func determineTimeZoneOffset(tm *pgTm, z pgZone) (int32, int64) {
	overflow := func() (int32, int64) {
		tm.isdst = 0
		return 0, 0
	}
	if !pgIsValidJulian(tm.year, tm.mon, tm.mday) {
		return overflow()
	}
	date := date2j(tm.year, tm.mon, tm.mday) - pgUnixEpochJDate
	day := int64(date) * pgSecsPerDay
	if day/pgSecsPerDay != int64(date) {
		return overflow()
	}
	sec := tm.sec + (tm.min+tm.hour*pgMinsPerHour)*pgSecsPerMinute
	mytime := day + int64(sec)
	if mytime < 0 && day > 0 {
		return overflow()
	}
	prevtime := mytime - pgSecsPerDay
	if mytime < 0 && prevtime > 0 {
		return overflow()
	}
	beforeOff, beforeDST, boundary, afterOff, afterDST, found := z.nextBoundary(prevtime)
	if !found {
		tm.isdst = beforeDST
		return int32(-beforeOff), mytime - beforeOff
	}
	beforetime := mytime - beforeOff
	if (beforeOff > 0 && mytime < 0 && beforetime > 0) || (beforeOff <= 0 && mytime > 0 && beforetime < 0) {
		return overflow()
	}
	aftertime := mytime - afterOff
	if (afterOff > 0 && mytime < 0 && aftertime > 0) || (afterOff <= 0 && mytime > 0 && aftertime < 0) {
		return overflow()
	}
	switch {
	case beforetime < boundary && aftertime < boundary:
		tm.isdst = beforeDST
		return int32(-beforeOff), beforetime
	case beforetime > boundary && aftertime >= boundary:
		tm.isdst = afterDST
		return int32(-afterOff), aftertime
	case beforetime > aftertime:
		tm.isdst = beforeDST
		return int32(-beforeOff), beforetime
	}
	tm.isdst = afterDST
	return int32(-afterOff), aftertime
}

// determineTimeZoneAbbrevOffset is DetermineTimeZoneAbbrevOffset.
func determineTimeZoneAbbrevOffset(tm *pgTm, abbr string, z pgZone) int32 {
	zoneOff, t := determineTimeZoneOffset(tm, z)
	if gmtoff, isdst, ok := z.interpretAbbrev(abbr, t); ok {
		tm.isdst = isdst
		return int32(-gmtoff)
	}
	return zoneOff
}

// zeroPad is pg_ultostr_zeropad: v in decimal, at least width digits.
func zeroPad(b *strings.Builder, v int64, width int) {
	s := strconv.FormatInt(v, 10)
	for i := len(s); i < width; i++ {
		b.WriteByte('0')
	}
	b.WriteString(s)
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// appendSeconds is AppendSeconds: sec, then the fraction to precision digits less trailing zeros.
func appendSeconds(b *strings.Builder, sec int32, fsec int64, precision int, fillzeros bool) {
	if fillzeros {
		zeroPad(b, int64(abs32(sec)), 2)
	} else {
		b.WriteString(strconv.FormatInt(int64(abs32(sec)), 10))
	}
	if fsec == 0 {
		return
	}
	value := abs64(fsec)
	digits := make([]byte, precision)
	end := precision
	got := false
	for p := precision - 1; p >= 0; p-- {
		old := value
		value /= 10
		r := old - value*10
		if r != 0 {
			got = true
		}
		if got {
			digits[p] = byte('0' + r)
		} else {
			end = p
		}
	}
	b.WriteByte('.')
	if value != 0 {
		b.WriteString(strconv.FormatInt(abs64(fsec), 10))
		return
	}
	b.Write(digits[:end])
}

// encodeTimezone is EncodeTimezone: tz (seconds west) as +hh[:mm[:ss]].
func encodeTimezone(b *strings.Builder, tz int32) {
	sec := abs32(tz)
	min := sec / pgSecsPerMinute
	sec -= min * pgSecsPerMinute
	hour := min / pgMinsPerHour
	min -= hour * pgMinsPerHour
	if tz <= 0 {
		b.WriteByte('+')
	} else {
		b.WriteByte('-')
	}
	zeroPad(b, int64(hour), 2)
	if sec != 0 {
		b.WriteByte(':')
		zeroPad(b, int64(min), 2)
		b.WriteByte(':')
		zeroPad(b, int64(sec), 2)
	} else if min != 0 {
		b.WriteByte(':')
		zeroPad(b, int64(min), 2)
	}
}

func pgYearOut(y int32) int64 {
	if y > 0 {
		return int64(y)
	}
	return -(int64(y) - 1)
}

// encodeDateOnly is EncodeDateOnly in the ISO style.
func encodeDateOnly(tm pgTm) string {
	var b strings.Builder
	zeroPad(&b, pgYearOut(tm.year), 4)
	b.WriteByte('-')
	zeroPad(&b, int64(tm.mon), 2)
	b.WriteByte('-')
	zeroPad(&b, int64(tm.mday), 2)
	if tm.year <= 0 {
		b.WriteString(" BC")
	}
	return b.String()
}

// encodeDateTime is EncodeDateTime in the ISO style.
func encodeDateTime(tm pgTm, fsec int64, printTZ bool, tz int32) string {
	if tm.isdst < 0 {
		printTZ = false
	}
	var b strings.Builder
	zeroPad(&b, pgYearOut(tm.year), 4)
	b.WriteByte('-')
	zeroPad(&b, int64(tm.mon), 2)
	b.WriteByte('-')
	zeroPad(&b, int64(tm.mday), 2)
	b.WriteByte(' ')
	zeroPad(&b, int64(tm.hour), 2)
	b.WriteByte(':')
	zeroPad(&b, int64(tm.min), 2)
	b.WriteByte(':')
	appendSeconds(&b, tm.sec, fsec, pgMaxTimestampPrecision, true)
	if printTZ {
		encodeTimezone(&b, tz)
	}
	if tm.year <= 0 {
		b.WriteString(" BC")
	}
	return b.String()
}

// encodeInterval is EncodeInterval in the postgres style.
func encodeInterval(tm pgTm, fsec int64) string {
	var b strings.Builder
	isBefore, isZero := false, true
	part := func(value int32, units string) {
		if value == 0 {
			return
		}
		if !isZero {
			b.WriteByte(' ')
		}
		if isBefore && value > 0 {
			b.WriteByte('+')
		}
		b.WriteString(strconv.FormatInt(int64(value), 10))
		b.WriteByte(' ')
		b.WriteString(units)
		if value != 1 {
			b.WriteByte('s')
		}
		isBefore = value < 0
		isZero = false
	}
	part(tm.year, "year")
	part(tm.mon, "mon")
	part(tm.mday, "day")
	if isZero || tm.hour != 0 || tm.min != 0 || tm.sec != 0 || fsec != 0 {
		minus := tm.hour < 0 || tm.min < 0 || tm.sec < 0 || fsec < 0
		if !isZero {
			b.WriteByte(' ')
		}
		if minus {
			b.WriteByte('-')
		} else if isBefore {
			b.WriteByte('+')
		}
		zeroPad(&b, int64(abs32(tm.hour)), 2)
		b.WriteByte(':')
		zeroPad(&b, int64(abs32(tm.min)), 2)
		b.WriteByte(':')
		appendSeconds(&b, tm.sec, fsec, pgMaxIntervalPrecision, true)
	}
	return b.String()
}

func (t pgTimestamp) String() string {
	switch int64(t) {
	case pgDTNoBegin:
		return "-infinity"
	case pgDTNoEnd:
		return "infinity"
	}
	tm, fsec, _, _, ok := timestamp2tm(int64(t), false, pgSessionZone)
	if !ok {
		return "timestamp out of range"
	}
	return encodeDateTime(tm, fsec, false, 0)
}

func (t pgTimestamptz) String() string {
	switch int64(t) {
	case pgDTNoBegin:
		return "-infinity"
	case pgDTNoEnd:
		return "infinity"
	}
	tm, fsec, tz, _, ok := timestamp2tm(int64(t), true, pgSessionZone)
	if !ok {
		return "timestamp out of range"
	}
	return encodeDateTime(tm, fsec, true, tz)
}

func (d pgDate) String() string {
	switch int32(d) {
	case math.MinInt32:
		return "-infinity"
	case math.MaxInt32:
		return "infinity"
	}
	var tm pgTm
	tm.year, tm.mon, tm.mday = j2date(int32(d) + pgPostgresEpochJDate)
	return encodeDateOnly(tm)
}

func (i pgInterval) String() string {
	tm, fsec, err := interval2tm(i)
	if err != nil {
		return "interval out of range"
	}
	return encodeInterval(tm, fsec)
}

// pgTimestamptzOf is the clock's time as a timestamptz.
func pgTimestamptzOf(t time.Time) pgTimestamptz {
	us := t.Unix()*pgUsecsPerSec + int64(t.Nanosecond()/1000)
	return pgTimestamptz(us - (pgPostgresEpochJDate-pgUnixEpochJDate)*pgSecsPerDay*pgUsecsPerSec)
}

// pgTZAbbrev is one timezone_abbreviations entry: a fixed offset (east of UTC), or a zone.
type pgTZAbbrev struct {
	abbr   string
	offset int32
	dst    bool
	zone   string
}
