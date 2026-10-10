package sqlfn

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SQLite's date and time functions: a line-for-line port of SQLite 3.53's date.c, the build stackql
// embeds. A moment is a Julian day number in milliseconds (iJD) with a calendar date (Y, M, D) and a
// time of day (h, m, s) derived from it and back by date.c's own formulas, so every rounding,
// rollover, range limit and modifier interaction is SQLite's. Text is read as SQLite reads it, as a
// C string: it ends at its first NUL.
//
// Two inputs come from the host, as they do for SQLite: the current time, and the local time zone
// that 'localtime' and 'utc' convert through (Go's time.Local, which reads the same TZ setting and
// zone database libc does).

type dateTime struct {
	iJD                int64
	Y, M, D            int
	h, m               int
	tz                 int // minutes east of UTC
	s                  float64
	validJD, validYMD  bool
	validHMS           bool
	nFloor             int64 // days to subtract for 'floor' after a month overflow
	rawS               bool  // s holds a bare number that 'unixepoch', 'julianday' or 'auto' may reinterpret
	isError, useSubsec bool
	isUTC, isLocal     bool
	clock              Clock // where "now" comes from
}

// at is s[i] as C reads a NUL-terminated string: 0 past the end.
func at(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// from is s[i:] as C pointer arithmetic sees it: empty past the end.
func from(s string, i int) string {
	if i < len(s) {
		return s[i:]
	}
	return ""
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func isSpaceByte(c byte) bool { return c == ' ' || (c >= '\t' && c <= '\r') }

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// aMx are getDigits' field maxima, indexed by the format letter a..f.
var aMx = [6]int{12, 14, 24, 31, 59, 14712}

// getDigits is date.c's getDigits: format is groups of four characters — digit count, minimum,
// maximum letter, and the separator that must follow (absent for the last group). It returns the
// values read; their count is how many groups matched.
func getDigits(z string, format string) []int {
	var out []int
	zi := 0
	for f := 0; ; f += 4 {
		n := int(format[f] - '0')
		lo := int(format[f+1] - '0')
		hi := aMx[format[f+2]-'a']
		var next byte
		if f+3 < len(format) {
			next = format[f+3]
		}
		val := 0
		for ; n > 0; n-- {
			c := at(z, zi)
			if !isDigitByte(c) {
				return out
			}
			val = val*10 + int(c-'0')
			zi++
		}
		if val < lo || val > hi || (next != 0 && next != at(z, zi)) {
			return out
		}
		out = append(out, val)
		zi++
		if next == 0 {
			return out
		}
	}
}

// parseTimezone reads an optional time zone — [+-]HH:MM or Z — then requires the end of the text.
// Like date.c it returns true on failure.
func (p *dateTime) parseTimezone(z string) bool {
	i := 0
	for isSpaceByte(at(z, i)) {
		i++
	}
	p.tz = 0
	sgn := 0
	switch c := at(z, i); c {
	case '-':
		sgn = -1
	case '+':
		sgn = 1
	case 'Z', 'z':
		i++
		p.isLocal, p.isUTC = false, true
		return p.zulu(z, i)
	default:
		return c != 0
	}
	i++
	v := getDigits(from(z, i), "20b:20e")
	if len(v) != 2 {
		return true
	}
	i += 5
	p.tz = sgn * (v[1] + v[0]*60)
	if p.tz == 0 {
		p.isLocal, p.isUTC = false, true
	}
	return p.zulu(z, i)
}

func (p *dateTime) zulu(z string, i int) bool {
	for isSpaceByte(at(z, i)) {
		i++
	}
	return at(z, i) != 0
}

// parseHhMmSs reads HH:MM[:SS[.FFFF]] and an optional time zone; true means failure.
func (p *dateTime) parseHhMmSs(z string) bool {
	v := getDigits(z, "20c:20e")
	if len(v) != 2 {
		return true
	}
	i := 5
	sec := 0
	ms := 0.0
	if at(z, i) == ':' {
		i++
		sv := getDigits(from(z, i), "20e")
		if len(sv) != 1 {
			return true
		}
		sec = sv[0]
		i += 2
		if at(z, i) == '.' && isDigitByte(at(z, i+1)) {
			scale := 1.0
			i++
			for isDigitByte(at(z, i)) {
				ms = float64(ms*10.0) + float64(at(z, i)) - '0'
				scale *= 10.0
				i++
			}
			ms /= scale
			// Truncated, to avoid sub-millisecond rounding.
			if ms > 0.999 {
				ms = 0.999
			}
		}
	}
	p.validJD = false
	p.rawS = false
	p.validHMS = true
	p.h, p.m = v[0], v[1]
	p.s = float64(sec) + ms
	return p.parseTimezone(from(z, i))
}

func (p *dateTime) datetimeError() { *p = dateTime{isError: true, clock: p.clock} }

func (p *dateTime) computeJD() {
	if p.validJD {
		return
	}
	y, mo, d := 2000, 1, 1
	if p.validYMD {
		y, mo, d = p.Y, p.M, p.D
	}
	if y < -4713 || y > 9999 || p.rawS {
		p.datetimeError()
		return
	}
	if mo <= 2 {
		y--
		mo += 12
	}
	a := (y + 4800) / 100
	b := 38 - a + a/4
	x1 := 36525 * (y + 4716) / 100
	x2 := 306001 * (mo + 1) / 10000
	p.iJD = int64((float64(x1+x2+d+b) - 1524.5) * 86400000)
	p.validJD = true
	if p.validHMS {
		p.iJD += int64(p.h*3600000+p.m*60000) + int64(float64(p.s*1000)+0.5)
		if p.tz != 0 {
			p.iJD -= int64(p.tz * 60000)
			p.validYMD, p.validHMS, p.tz = false, false, 0
			p.isUTC, p.isLocal = true, false
		}
	}
}

// computeFloor records how many days a day-of-month past its month's end overflowed by.
func (p *dateTime) computeFloor() {
	switch {
	case p.D <= 28:
		p.nFloor = 0
	case (1<<p.M)&0x15aa != 0:
		p.nFloor = 0
	case p.M != 2:
		p.nFloor = 0
		if p.D == 31 {
			p.nFloor = 1
		}
	case p.Y%4 != 0 || (p.Y%100 == 0 && p.Y%400 != 0):
		p.nFloor = int64(p.D - 28)
	default:
		p.nFloor = int64(p.D - 29)
	}
}

// parseYyyyMmDd reads [-]YYYY-MM-DD, then an optional time after spaces or a T; true means failure.
func (p *dateTime) parseYyyyMmDd(z string) bool {
	neg := false
	if at(z, 0) == '-' {
		z = z[1:]
		neg = true
	}
	v := getDigits(z, "40f-21a-21d")
	if len(v) != 3 {
		return true
	}
	i := 10
	for isSpaceByte(at(z, i)) || at(z, i) == 'T' {
		i++
	}
	if !p.parseHhMmSs(from(z, i)) {
		// the time was read
	} else if at(z, i) == 0 {
		p.validHMS = false
	} else {
		return true
	}
	p.validJD = false
	p.validYMD = true
	p.Y = v[0]
	if neg {
		p.Y = -v[0]
	}
	p.M, p.D = v[1], v[2]
	p.computeFloor()
	if p.tz != 0 {
		p.computeJD()
	}
	return false
}

// julianMillis is t as a Julian day in milliseconds, as SQLite's xCurrentTimeInt64 gives it.
func julianMillis(t time.Time) int64 { return t.UnixMilli() + 210866760000000 }

func (p *dateTime) setDateTimeToCurrent() bool {
	p.iJD = julianMillis(p.clock.Now())
	if p.iJD > 0 {
		p.validJD = true
		p.isUTC, p.isLocal = true, false
		p.clearYMDHMSTZ()
		return false
	}
	return true
}

func (p *dateTime) setRawDateNumber(r float64) {
	p.s = r
	p.rawS = true
	if r >= 0.0 && r < 5373484.5 {
		p.iJD = int64(float64(r*86400000.0) + 0.5)
		p.validJD = true
	}
}

// parseDateOrTime reads a time value given as text; true means failure.
func (p *dateTime) parseDateOrTime(z string) bool {
	if !p.parseYyyyMmDd(z) {
		return false
	}
	if !p.parseHhMmSs(z) {
		return false
	}
	if strings.EqualFold(z, "now") {
		return p.setDateTimeToCurrent()
	}
	if r, ok := sqliteAtoF(z); ok {
		p.setRawDateNumber(r)
		return false
	}
	if strings.EqualFold(z, "subsec") || strings.EqualFold(z, "subsecond") {
		p.useSubsec = true
		return p.setDateTimeToCurrent()
	}
	return true
}

func validJulianDay(iJD int64) bool { return iJD >= 0 && iJD <= 464269060799999 }

func (p *dateTime) computeYMD() {
	if p.validYMD {
		return
	}
	if !p.validJD {
		p.Y, p.M, p.D = 2000, 1, 1
	} else if !validJulianDay(p.iJD) {
		p.datetimeError()
		return
	} else {
		z := int((p.iJD + 43200000) / 86400000)
		alpha := int((float64(z)+32044.75)/36524.25) - 52
		a := z + 1 + alpha - (alpha+100)/4 + 25
		b := a + 1524
		c := int((float64(b) - 122.1) / 365.25)
		d := 36525 * (c & 32767) / 100
		e := int(float64(b-d) / 30.6001)
		x1 := int(30.6001 * float64(e))
		p.D = b - d - x1
		if e < 14 {
			p.M = e - 1
		} else {
			p.M = e - 13
		}
		if p.M > 2 {
			p.Y = c - 4716
		} else {
			p.Y = c - 4715
		}
	}
	p.validYMD = true
}

func (p *dateTime) computeHMS() {
	if p.validHMS {
		return
	}
	p.computeJD()
	dayMs := int((p.iJD + 43200000) % 86400000)
	p.s = float64(dayMs%60000) / 1000.0
	dayMin := dayMs / 60000
	p.m = dayMin % 60
	p.h = dayMin / 60
	p.rawS = false
	p.validHMS = true
}

func (p *dateTime) computeYMDHMS() { p.computeYMD(); p.computeHMS() }

func (p *dateTime) clearYMDHMSTZ() { p.validYMD, p.validHMS, p.tz = false, false, 0 }

// toLocaltime converts p from UTC to the host's local time. Outside 1970..2037 the year is mapped to
// an equivalent one inside it and back, as SQLite does around localtime_r.
func (p *dateTime) toLocaltime() {
	p.computeJD()
	var t int64
	yearDiff := 0
	if p.iJD < 2108667600*100000 || p.iJD > 2130141456*100000 {
		x := *p
		x.computeYMDHMS()
		yearDiff = 2000 + x.Y%4 - x.Y
		x.Y += yearDiff
		x.validJD = false
		x.computeJD()
		t = x.iJD/1000 - 21086676*10000
	} else {
		t = p.iJD/1000 - 21086676*10000
	}
	lt := time.Unix(t, 0).In(time.Local)
	p.Y = lt.Year() - yearDiff
	p.M = int(lt.Month())
	p.D = lt.Day()
	p.h = lt.Hour()
	p.m = lt.Minute()
	p.s = float64(lt.Second()) + float64(float64(p.iJD%1000)*0.001)
	p.validYMD, p.validHMS = true, true
	p.validJD = false
	p.rawS = false
	p.tz = 0
	p.isError = false
}

// autoAdjustDate is 'auto': a number in the Julian-day range stays one; otherwise one in the Unix
// range is Unix seconds.
func (p *dateTime) autoAdjustDate() {
	if !p.rawS || p.validJD {
		p.rawS = false
	} else if p.s >= -21086676*10000 && p.s <= 25340230*10000+799 {
		r := float64(p.s*1000.0) + 210866760000000
		p.clearYMDHMSTZ()
		p.iJD = int64(r + 0.5)
		p.validJD = true
		p.rawS = false
	}
}

// xformType are the '±N unit' units: name, magnitude limit and seconds per unit, in single precision
// as date.c stores them.
var xformType = [6]struct {
	name  string
	limit float32
	xform float32
}{
	{"second", 4.6427e+14, 1},
	{"minute", 7.7379e+12, 60},
	{"hour", 1.2897e+11, 3600},
	{"day", 5.373485e+06, 86400},
	{"month", 176546, 2.592e+06},
	{"year", 14713, 3.1536e+07},
}

// normalizeMonth carries a month outside 1..12 into the year.
func (p *dateTime) normalizeMonth() {
	var x int
	if p.M > 0 {
		x = (p.M - 1) / 12
	} else {
		x = (p.M - 12) / 12
	}
	p.Y += x
	p.M -= x * 12
}

// parseModifier applies one modifier; true means failure.
func (p *dateTime) parseModifier(z string, idx int) bool {
	rc := true
	switch lowerASCII(at(z, 0)) {
	case 'a':
		if strings.EqualFold(z, "auto") {
			if idx > 1 {
				return true
			}
			p.autoAdjustDate()
			rc = false
		}
	case 'c':
		if strings.EqualFold(z, "ceiling") {
			p.computeJD()
			p.clearYMDHMSTZ()
			rc = false
			p.nFloor = 0
		}
	case 'f':
		if strings.EqualFold(z, "floor") {
			p.computeJD()
			p.iJD -= p.nFloor * 86400000
			p.clearYMDHMSTZ()
			rc = false
		}
	case 'j':
		if strings.EqualFold(z, "julianday") {
			if idx > 1 {
				return true
			}
			if p.validJD && p.rawS {
				rc = false
				p.rawS = false
			}
		}
	case 'l':
		if strings.EqualFold(z, "localtime") {
			if !p.isLocal {
				p.toLocaltime()
			}
			rc = false
			p.isUTC, p.isLocal = false, true
		}
	case 'u':
		if strings.EqualFold(z, "unixepoch") && p.rawS {
			if idx > 1 {
				return true
			}
			r := float64(p.s*1000.0) + 2.1086676e+14
			if r >= 0 && r < 4.642690608e+14 {
				p.clearYMDHMSTZ()
				p.iJD = int64(r + 0.5)
				p.validJD = true
				p.rawS = false
				rc = false
			}
		} else if strings.EqualFold(z, "utc") {
			if !p.isUTC {
				// Guess the UTC moment whose local time is p, refining up to three times.
				p.computeJD()
				orig := p.iJD
				guess := orig
				var diff int64
				for cnt := 0; ; {
					guess -= diff
					n := dateTime{iJD: guess, validJD: true}
					n.toLocaltime()
					n.computeJD()
					diff = n.iJD - orig
					if diff == 0 {
						break
					}
					c := cnt
					cnt++
					if c >= 3 {
						break
					}
				}
				*p = dateTime{iJD: guess, validJD: true, isUTC: true, clock: p.clock}
			}
			rc = false
		}
	case 'w':
		if len(z) >= 8 && strings.EqualFold(z[:8], "weekday ") {
			r, ok := sqliteAtoF(z[8:])
			if ok && r >= 0 && r < 7 {
				n := int(r)
				if float64(n) == r {
					p.computeYMDHMS()
					p.tz = 0
					p.validJD = false
					p.computeJD()
					d := (p.iJD + 129600000) / 86400000 % 7
					if d > int64(n) {
						d -= 7
					}
					p.iJD += (int64(n) - d) * 86400000
					p.clearYMDHMSTZ()
					rc = false
				}
			}
		}
	case 's':
		if len(z) < 9 || !strings.EqualFold(z[:9], "start of ") {
			if strings.EqualFold(z, "subsec") || strings.EqualFold(z, "subsecond") {
				p.useSubsec = true
				rc = false
			}
			break
		}
		if !p.validJD && !p.validYMD && !p.validHMS {
			break
		}
		z = z[9:]
		p.computeYMD()
		p.validHMS = true
		p.h, p.m, p.s = 0, 0, 0
		p.rawS = false
		p.tz = 0
		p.validJD = false
		switch {
		case strings.EqualFold(z, "month"):
			p.D = 1
			rc = false
		case strings.EqualFold(z, "year"):
			p.M, p.D = 1, 1
			rc = false
		case strings.EqualFold(z, "day"):
			rc = false
		}
	case '+', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		rc = p.numericModifier(z)
	}
	return rc
}

// numericModifier is '±N unit', '±HH:MM[:SS[.FFF]]' and '±YYYY-MM-DD[ HH:MM[:SS[.FFF]]]'; true means
// failure.
func (p *dateTime) numericModifier(z string) bool {
	z2 := z
	z0 := at(z, 0)
	n := 1
	for ; at(z, n) != 0; n++ {
		c := at(z, n)
		if c == ':' || isSpaceByte(c) {
			break
		}
		if c == '-' {
			if n == 5 && len(getDigits(z[1:], "40f")) == 1 {
				break
			}
			if n == 6 && len(getDigits(z[1:], "50f")) == 1 {
				break
			}
		}
	}
	r, ok := sqliteAtoF(z[:n])
	if !ok {
		return true
	}
	if at(z, n) == '-' {
		// ±YYYY-MM-DD adds or subtracts years, months (0..11) and days (0..30).
		if z0 != '+' && z0 != '-' {
			return true
		}
		var v []int
		if n == 5 {
			v = getDigits(z[1:], "40f-20a-20d")
		} else {
			v = getDigits(z[1:], "50f-20a-20d")
			z = z[1:]
		}
		if len(v) != 3 {
			return true
		}
		y, mo, d := v[0], v[1], v[2]
		if mo >= 12 || d >= 31 {
			return true
		}
		p.computeYMDHMS()
		p.validJD = false
		if z0 == '-' {
			p.Y -= y
			p.M -= mo
			d = -d
		} else {
			p.Y += y
			p.M += mo
		}
		p.normalizeMonth()
		p.computeFloor()
		p.computeJD()
		p.validHMS, p.validYMD = false, false
		p.iJD += int64(d) * 86400000
		if at(z, 11) == 0 {
			return false
		}
		if !isSpaceByte(at(z, 11)) || len(getDigits(from(z, 12), "20c:20e")) != 2 {
			return true
		}
		z2 = z[12:]
		n = 2
	}
	if at(z2, n) == ':' {
		// ±HH:MM[:SS[.FFF]] shifts by a time of day.
		if !isDigitByte(at(z2, 0)) {
			z2 = z2[1:]
		}
		var tx dateTime
		if tx.parseHhMmSs(z2) {
			return true
		}
		tx.computeJD()
		tx.iJD -= 43200000
		day := tx.iJD / 86400000
		tx.iJD -= day * 86400000
		if z0 == '-' {
			tx.iJD = -tx.iJD
		}
		p.computeJD()
		p.clearYMDHMSTZ()
		p.iJD += tx.iJD
		return false
	}
	// ±N unit[s]
	z = z[n:]
	for isSpaceByte(at(z, 0)) {
		z = z[1:]
	}
	n = len(z)
	if n < 3 || n > 10 {
		return true
	}
	if lowerASCII(z[n-1]) == 's' {
		n--
	}
	p.computeJD()
	rounder := 0.5
	if r < 0 {
		rounder = -0.5
	}
	p.nFloor = 0
	rc := true
	for i, u := range xformType {
		if len(u.name) != n || !strings.EqualFold(u.name, z[:n]) || !(r > float64(-u.limit) && r < float64(u.limit)) {
			continue
		}
		switch i {
		case 4: // months
			p.computeYMDHMS()
			p.M += int(r)
			p.normalizeMonth()
			p.computeFloor()
			p.validJD = false
			r -= float64(int(r))
		case 5: // years
			y := int(r)
			p.computeYMDHMS()
			p.Y += y
			p.computeFloor()
			p.validJD = false
			r -= float64(int(r))
		}
		p.computeJD()
		p.iJD += int64(float64(float64(r*1000)*float64(u.xform)) + rounder)
		rc = false
		break
	}
	p.clearYMDHMSTZ()
	return rc
}

// isDate reads a function's arguments — a time value (now when there is none), then each modifier —
// into a moment; false where SQLite returns NULL.
func isDate(a []any, clock Clock) (*dateTime, bool) {
	p := &dateTime{clock: clock}
	if len(a) == 0 {
		return p, !p.setDateTimeToCurrent()
	}
	switch c, x := norm(a[0]); c {
	case classNull:
		return nil, false
	case classInteger, classReal:
		p.setRawDateNumber(sqlReal(x))
	default:
		if p.parseDateOrTime(beforeNUL(sqlText(x))) {
			return nil, false
		}
	}
	for i := 1; i < len(a); i++ {
		if isNull(a[i]) || p.parseModifier(beforeNUL(sqlText(a[i])), i) {
			return nil, false
		}
	}
	p.computeJD()
	if p.isError || !validJulianDay(p.iJD) {
		return nil, false
	}
	if len(a) == 1 && p.validYMD && p.D > 28 {
		// A lone YYYY-MM-DD is normalized: 2023-02-31 is 2023-03-03.
		p.validYMD = false
	}
	return p, true
}

// sqliteAtoF is sqlite3AtoF where it returns positive: the text, less surrounding whitespace, is
// wholly a decimal number.
func sqliteAtoF(z string) (float64, bool) {
	f, rc := sqliteAtoFRaw(z)
	return f, rc > 0
}

func (p *dateTime) dateText() string {
	y := p.Y
	if y < 0 {
		y = -y
	}
	s := fmt.Sprintf("%d%d%d%d-%d%d-%d%d", y/1000%10, y/100%10, y/10%10, y%10, p.M/10%10, p.M%10, p.D/10%10, p.D%10)
	if p.Y < 0 {
		return "-" + s
	}
	return s
}

func (p *dateTime) timeText() string {
	s := fmt.Sprintf("%d%d:%d%d:", p.h/10%10, p.h%10, p.m/10%10, p.m%10)
	if p.useSubsec {
		ms := int(float64(1000*p.s) + 0.5)
		return s + fmt.Sprintf("%d%d.%d%d%d", ms/10000%10, ms/1000%10, ms/100%10, ms/10%10, ms%10)
	}
	sec := int(p.s)
	return s + fmt.Sprintf("%d%d", sec/10%10, sec%10)
}

func sqliteDates(clock Clock) []Func {
	dated := func(name string, f func(p *dateTime) any) Func {
		return NewScalar(name, 0, -1, func(a []any) (any, error) {
			p, ok := isDate(a, clock)
			if !ok {
				return nil, nil
			}
			return f(p), nil
		})
	}
	now := func(name string, f func(p *dateTime) any) Func {
		return NewScalar(name, 0, 0, func([]any) (any, error) {
			p, ok := isDate(nil, clock)
			if !ok {
				return nil, nil
			}
			return f(p), nil
		})
	}
	date := func(p *dateTime) any { p.computeYMD(); return p.dateText() }
	tm := func(p *dateTime) any { p.computeHMS(); return p.timeText() }
	datetime := func(p *dateTime) any { p.computeYMDHMS(); return p.dateText() + " " + p.timeText() }
	return []Func{
		dated("julianday", func(p *dateTime) any { p.computeJD(); return float64(p.iJD) / 86400000.0 }),
		dated("unixepoch", func(p *dateTime) any {
			p.computeJD()
			if p.useSubsec {
				return float64(p.iJD-21086676*10000000) / 1000.0
			}
			return p.iJD/1000 - 21086676*10000
		}),
		dated("datetime", datetime),
		dated("date", date),
		dated("time", tm),
		now("current_date", date),
		now("current_time", tm),
		now("current_timestamp", datetime),
		NewScalar("strftime", 1, -1, func(a []any) (any, error) {
			if isNull(a[0]) {
				return nil, nil
			}
			p, ok := isDate(a[1:], clock)
			if !ok {
				return nil, nil
			}
			return strftime(beforeNUL(sqlText(a[0])), p), nil
		}),
		NewScalar("timediff", 2, 2, func(a []any) (any, error) { return timediff(a, clock) }),
	}
}

func (p *dateTime) daysAfterJan01() int {
	jan01 := *p
	jan01.validJD = false
	jan01.M, jan01.D = 1, 1
	jan01.computeJD()
	return int((p.iJD - jan01.iJD + 43200000) / 86400000)
}

func (p *dateTime) daysAfterMonday() int { return int((p.iJD+43200000)/86400000) % 7 }

func (p *dateTime) daysAfterSunday() int { return int((p.iJD+129600000)/86400000) % 7 }

// isoThursday is the Thursday of p's ISO week.
func (p *dateTime) isoThursday() dateTime {
	y := *p
	y.iJD += int64((3 - p.daysAfterMonday()) * 86400000)
	y.validYMD = false
	y.computeYMD()
	return y
}

// strftime renders p by format; an unknown conversion makes the result NULL.
func strftime(format string, p *dateTime) any {
	p.computeJD()
	p.computeYMDHMS()
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			b.WriteByte(format[i])
			continue
		}
		i++
		switch cf := at(format, i); cf {
		case 'd':
			fmt.Fprintf(&b, "%02d", p.D)
		case 'e':
			fmt.Fprintf(&b, "%2d", p.D)
		case 'f':
			s := p.s
			if s > 59.999 {
				s = 59.999
			}
			fmt.Fprintf(&b, "%06.3f", s)
		case 'F':
			fmt.Fprintf(&b, "%04d-%02d-%02d", p.Y, p.M, p.D)
		case 'G':
			y := p.isoThursday()
			fmt.Fprintf(&b, "%04d", y.Y)
		case 'g':
			y := p.isoThursday()
			fmt.Fprintf(&b, "%02d", y.Y%100)
		case 'H':
			fmt.Fprintf(&b, "%02d", p.h)
		case 'k':
			fmt.Fprintf(&b, "%2d", p.h)
		case 'I', 'l':
			h := p.h
			if h > 12 {
				h -= 12
			}
			if h == 0 {
				h = 12
			}
			if cf == 'I' {
				fmt.Fprintf(&b, "%02d", h)
			} else {
				fmt.Fprintf(&b, "%2d", h)
			}
		case 'j':
			fmt.Fprintf(&b, "%03d", p.daysAfterJan01()+1)
		case 'J':
			b.WriteString(strconv.FormatFloat(float64(p.iJD)/86400000.0, 'g', 16, 64))
		case 'm':
			fmt.Fprintf(&b, "%02d", p.M)
		case 'M':
			fmt.Fprintf(&b, "%02d", p.m)
		case 'p', 'P':
			s := "AM"
			if p.h >= 12 {
				s = "PM"
			}
			if cf == 'P' {
				s = strings.ToLower(s)
			}
			b.WriteString(s)
		case 'R':
			fmt.Fprintf(&b, "%02d:%02d", p.h, p.m)
		case 's':
			if p.useSubsec {
				fmt.Fprintf(&b, "%.3f", float64(p.iJD-21086676*10000000)/1000.0)
			} else {
				fmt.Fprintf(&b, "%d", p.iJD/1000-21086676*10000)
			}
		case 'S':
			fmt.Fprintf(&b, "%02d", int(p.s))
		case 'T':
			fmt.Fprintf(&b, "%02d:%02d:%02d", p.h, p.m, int(p.s))
		case 'u', 'w':
			c := byte(p.daysAfterSunday()) + '0'
			if c == '0' && cf == 'u' {
				c = '7'
			}
			b.WriteByte(c)
		case 'U':
			fmt.Fprintf(&b, "%02d", (p.daysAfterJan01()-p.daysAfterSunday()+7)/7)
		case 'V':
			y := p.isoThursday()
			fmt.Fprintf(&b, "%02d", y.daysAfterJan01()/7+1)
		case 'W':
			fmt.Fprintf(&b, "%02d", (p.daysAfterJan01()-p.daysAfterMonday()+7)/7)
		case 'Y':
			fmt.Fprintf(&b, "%04d", p.Y)
		case '%':
			b.WriteByte('%')
		default:
			return nil
		}
	}
	return b.String()
}

// timediff is timediff(A, B): the time from B to A as ±YYYY-MM-DD HH:MM:SS.SSS — whole years and
// months stepped on the calendar first, then what remains.
func timediff(a []any, clock Clock) (any, error) {
	d1, ok := isDate(a[:1], clock)
	if !ok {
		return nil, nil
	}
	d2, ok := isDate(a[1:2], clock)
	if !ok {
		return nil, nil
	}
	d1.computeYMDHMS()
	d2.computeYMDHMS()
	var sign byte
	var y, m int
	if d1.iJD >= d2.iJD {
		sign = '+'
		y = d1.Y - d2.Y
		if y != 0 {
			d2.Y = d1.Y
			d2.validJD = false
			d2.computeJD()
		}
		m = d1.M - d2.M
		if m < 0 {
			y--
			m += 12
		}
		if m != 0 {
			d2.M = d1.M
			d2.validJD = false
			d2.computeJD()
		}
		for d1.iJD < d2.iJD {
			m--
			if m < 0 {
				m = 11
				y--
			}
			d2.M--
			if d2.M < 1 {
				d2.M = 12
				d2.Y--
			}
			d2.validJD = false
			d2.computeJD()
		}
		d1.iJD -= d2.iJD
	} else {
		sign = '-'
		y = d2.Y - d1.Y
		if y != 0 {
			d2.Y = d1.Y
			d2.validJD = false
			d2.computeJD()
		}
		m = d2.M - d1.M
		if m < 0 {
			y--
			m += 12
		}
		if m != 0 {
			d2.M = d1.M
			d2.validJD = false
			d2.computeJD()
		}
		for d1.iJD > d2.iJD {
			m--
			if m < 0 {
				m = 11
				y--
			}
			d2.M++
			if d2.M > 12 {
				d2.M = 1
				d2.Y++
			}
			d2.validJD = false
			d2.computeJD()
		}
		d1.iJD = d2.iJD - d1.iJD
	}
	d1.iJD += 1486995408 * 100000
	d1.clearYMDHMSTZ()
	d1.computeYMDHMS()
	return fmt.Sprintf("%c%04d-%02d-%02d %02d:%02d:%06.3f", sign, y, m, d1.D-1, d1.h, d1.m, d1.s), nil
}
