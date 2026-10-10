package sqlfn

import (
	"math"
	"strconv"
	"strings"
)

// datetime.c's parsing of date, time and interval input.

// Field types and token types (datetime.h).
const (
	dtkNumber     = 0
	dtkString     = 1
	dtkDate       = 2
	dtkTime       = 3
	dtkTZ         = 4
	dtkAgo        = 5
	dtkSpecial    = 6
	dtkEarly      = 9
	dtkLate       = 10
	dtkEpoch      = 11
	dtkNow        = 12
	dtkYesterday  = 13
	dtkToday      = 14
	dtkTomorrow   = 15
	dtkZulu       = 16
	dtkDelta      = 17
	dtkSecond     = 18
	dtkMinute     = 19
	dtkHour       = 20
	dtkDay        = 21
	dtkWeek       = 22
	dtkMonth      = 23
	dtkQuarter    = 24
	dtkYear       = 25
	dtkDecade     = 26
	dtkCentury    = 27
	dtkMillennium = 28
	dtkMillisec   = 29
	dtkMicrosec   = 30
	dtkJulian     = 31
	dtkDow        = 32
	dtkDoy        = 33
	dtkTZHour     = 34
	dtkTZMinute   = 35
	dtkISOYear    = 36
	dtkISODow     = 37

	tokReserv      = 0
	tokMonth       = 1
	tokYear        = 2
	tokDay         = 3
	tokJulian      = 4
	tokTZ          = 5
	tokDTZ         = 6
	tokDynTZ       = 7
	tokIgnore      = 8
	tokAMPM        = 9
	tokHour        = 10
	tokMinute      = 11
	tokSecond      = 12
	tokMillisecond = 13
	tokMicrosecond = 14
	tokDOY         = 15
	tokDOW         = 16
	tokUnits       = 17
	tokADBC        = 18
	tokAgo         = 19
	tokISODate     = 22
	tokISOTime     = 23
	tokWeek        = 24
	tokDecade      = 25
	tokCentury     = 26
	tokMillennium  = 27
	tokDTZMod      = 28
	tokUnknown     = 31

	dtAM   = 0
	dtPM   = 1
	dtHR24 = 2
	dtAD   = 0
	dtBC   = 1

	dtErrBadFormat        = -1
	dtErrFieldOverflow    = -2
	dtErrMDFieldOverflow  = -3
	dtErrIntervalOverflow = -4
	dtErrTZDispOverflow   = -5

	pgMaxDateLen    = 128
	pgMaxDateFields = 25
	pgTokMaxLen     = 10

	pgIntervalFullRange = 0x7FFF
)

func dtkM(t int) int { return 1 << t }

var (
	dtkAllSecsM = dtkM(tokSecond) | dtkM(tokMillisecond) | dtkM(tokMicrosecond)
	dtkDateM    = dtkM(tokYear) | dtkM(tokMonth) | dtkM(tokDay)
	dtkTimeM    = dtkM(tokHour) | dtkM(tokMinute) | dtkAllSecsM
)

// pgDatetkn is a datetkn.
type pgDatetkn struct {
	token string
	typ   int
	value int32
}

var pgDatetktbl = []pgDatetkn{
	{"-infinity", tokReserv, dtkEarly}, {"ad", tokADBC, dtAD}, {"allballs", tokReserv, dtkZulu},
	{"am", tokAMPM, dtAM}, {"apr", tokMonth, 4}, {"april", tokMonth, 4}, {"at", tokIgnore, 0},
	{"aug", tokMonth, 8}, {"august", tokMonth, 8}, {"bc", tokADBC, dtBC}, {"d", tokUnits, dtkDay},
	{"dec", tokMonth, 12}, {"december", tokMonth, 12}, {"dow", tokUnits, dtkDow}, {"doy", tokUnits, dtkDoy},
	{"dst", tokDTZMod, pgSecsPerHour}, {"epoch", tokReserv, dtkEpoch}, {"feb", tokMonth, 2},
	{"february", tokMonth, 2}, {"fri", tokDOW, 5}, {"friday", tokDOW, 5}, {"h", tokUnits, dtkHour},
	{"infinity", tokReserv, dtkLate}, {"isodow", tokUnits, dtkISODow}, {"isoyear", tokUnits, dtkISOYear},
	{"j", tokUnits, dtkJulian}, {"jan", tokMonth, 1}, {"january", tokMonth, 1}, {"jd", tokUnits, dtkJulian},
	{"jul", tokMonth, 7}, {"julian", tokUnits, dtkJulian}, {"july", tokMonth, 7}, {"jun", tokMonth, 6},
	{"june", tokMonth, 6}, {"m", tokUnits, dtkMonth}, {"mar", tokMonth, 3}, {"march", tokMonth, 3},
	{"may", tokMonth, 5}, {"mm", tokUnits, dtkMinute}, {"mon", tokDOW, 1}, {"monday", tokDOW, 1},
	{"nov", tokMonth, 11}, {"november", tokMonth, 11}, {"now", tokReserv, dtkNow}, {"oct", tokMonth, 10},
	{"october", tokMonth, 10}, {"on", tokIgnore, 0}, {"pm", tokAMPM, dtPM}, {"s", tokUnits, dtkSecond},
	{"sat", tokDOW, 6}, {"saturday", tokDOW, 6}, {"sep", tokMonth, 9}, {"sept", tokMonth, 9},
	{"september", tokMonth, 9}, {"sun", tokDOW, 0}, {"sunday", tokDOW, 0}, {"t", tokISOTime, dtkTime},
	{"thu", tokDOW, 4}, {"thur", tokDOW, 4}, {"thurs", tokDOW, 4}, {"thursday", tokDOW, 4},
	{"today", tokReserv, dtkToday}, {"tomorrow", tokReserv, dtkTomorrow}, {"tue", tokDOW, 2},
	{"tues", tokDOW, 2}, {"tuesday", tokDOW, 2}, {"wed", tokDOW, 3}, {"wednesday", tokDOW, 3},
	{"weds", tokDOW, 3}, {"y", tokUnits, dtkYear}, {"yesterday", tokReserv, dtkYesterday},
}

var pgDeltatktbl = []pgDatetkn{
	{"@", tokIgnore, 0}, {"ago", tokAgo, 0}, {"c", tokUnits, dtkCentury}, {"cent", tokUnits, dtkCentury},
	{"centuries", tokUnits, dtkCentury}, {"century", tokUnits, dtkCentury}, {"d", tokUnits, dtkDay},
	{"day", tokUnits, dtkDay}, {"days", tokUnits, dtkDay}, {"dec", tokUnits, dtkDecade},
	{"decade", tokUnits, dtkDecade}, {"decades", tokUnits, dtkDecade}, {"decs", tokUnits, dtkDecade},
	{"h", tokUnits, dtkHour}, {"hour", tokUnits, dtkHour}, {"hours", tokUnits, dtkHour}, {"hr", tokUnits, dtkHour},
	{"hrs", tokUnits, dtkHour}, {"m", tokUnits, dtkMinute}, {"microsecon", tokUnits, dtkMicrosec},
	{"mil", tokUnits, dtkMillennium}, {"millennia", tokUnits, dtkMillennium}, {"millennium", tokUnits, dtkMillennium},
	{"millisecon", tokUnits, dtkMillisec}, {"mils", tokUnits, dtkMillennium}, {"min", tokUnits, dtkMinute},
	{"mins", tokUnits, dtkMinute}, {"minute", tokUnits, dtkMinute}, {"minutes", tokUnits, dtkMinute},
	{"mon", tokUnits, dtkMonth}, {"mons", tokUnits, dtkMonth}, {"month", tokUnits, dtkMonth},
	{"months", tokUnits, dtkMonth}, {"ms", tokUnits, dtkMillisec}, {"msec", tokUnits, dtkMillisec},
	{"msecond", tokUnits, dtkMillisec}, {"mseconds", tokUnits, dtkMillisec}, {"msecs", tokUnits, dtkMillisec},
	{"qtr", tokUnits, dtkQuarter}, {"quarter", tokUnits, dtkQuarter}, {"s", tokUnits, dtkSecond},
	{"sec", tokUnits, dtkSecond}, {"second", tokUnits, dtkSecond}, {"seconds", tokUnits, dtkSecond},
	{"secs", tokUnits, dtkSecond}, {"timezone", tokUnits, dtkTZ}, {"timezone_h", tokUnits, dtkTZHour},
	{"timezone_m", tokUnits, dtkTZMinute}, {"us", tokUnits, dtkMicrosec}, {"usec", tokUnits, dtkMicrosec},
	{"usecond", tokUnits, dtkMicrosec}, {"useconds", tokUnits, dtkMicrosec}, {"usecs", tokUnits, dtkMicrosec},
	{"w", tokUnits, dtkWeek}, {"week", tokUnits, dtkWeek}, {"weeks", tokUnits, dtkWeek}, {"y", tokUnits, dtkYear},
	{"year", tokUnits, dtkYear}, {"years", tokUnits, dtkYear}, {"yr", tokUnits, dtkYear}, {"yrs", tokUnits, dtkYear},
}

// strncmp10 is strncmp(a, b, TOKMAXLEN) over strings with C's terminating NUL.
func strncmp10(a, b string) int {
	for i := 0; i < pgTokMaxLen; i++ {
		ca, cb := byte(0), byte(0)
		if i < len(a) {
			ca = a[i]
		}
		if i < len(b) {
			cb = b[i]
		}
		if ca != cb {
			return int(ca) - int(cb)
		}
		if ca == 0 {
			return 0
		}
	}
	return 0
}

// datebsearch is datebsearch: the token key names, compared on its first TOKMAXLEN characters.
func datebsearch(key string, tbl []pgDatetkn) (pgDatetkn, bool) {
	lo, hi := 0, len(tbl)-1
	for lo <= hi {
		mid := lo + (hi-lo)>>1
		k0, t0 := 0, 0
		if key != "" {
			k0 = int(key[0])
		}
		if tbl[mid].token != "" {
			t0 = int(tbl[mid].token[0])
		}
		r := k0 - t0
		if r == 0 {
			r = strncmp10(key, tbl[mid].token)
			if r == 0 {
				return tbl[mid], true
			}
		}
		if r < 0 {
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	return pgDatetkn{}, false
}

func decodeSpecial(low string) (int, int32) {
	if t, ok := datebsearch(low, pgDatetktbl); ok {
		return t.typ, t.value
	}
	return tokUnknown, 0
}

func decodeUnits(low string) (int, int32) {
	if t, ok := datebsearch(low, pgDeltatktbl); ok {
		return t.typ, t.value
	}
	return tokUnknown, 0
}

// pgAbbrevTable is the Default abbreviations as datetkns, sorted.
var pgAbbrevTable = func() []pgDatetkn {
	out := make([]pgDatetkn, len(pgTZAbbrevs))
	for i, a := range pgTZAbbrevs {
		tok := a.abbr
		if len(tok) > pgTokMaxLen {
			tok = tok[:pgTokMaxLen]
		}
		switch {
		case a.zone != "":
			out[i] = pgDatetkn{tok, tokDynTZ, int32(i)}
		case a.dst:
			out[i] = pgDatetkn{tok, tokDTZ, a.offset}
		default:
			out[i] = pgDatetkn{tok, tokTZ, a.offset}
		}
	}
	return out
}()

// decodeTimezoneAbbrev is DecodeTimezoneAbbrev: an abbreviation's type, its offset east, and for a
// dynamic one its zone.
func decodeTimezoneAbbrev(low string) (int, int32, pgZone, bool) {
	t, ok := datebsearch(low, pgAbbrevTable)
	if !ok {
		return tokUnknown, 0, pgZone{}, false
	}
	if t.typ == tokDynTZ {
		z, ok := pgTzset(pgTZAbbrevs[t.value].zone)
		if !ok {
			return tokUnknown, 0, pgZone{}, false
		}
		return t.typ, 0, z, true
	}
	return t.typ, t.value, pgZone{}, true
}

func cIsSpace(c byte) bool { return c == ' ' || (c >= '\t' && c <= '\r') }
func cIsDigit(c byte) bool { return c >= '0' && c <= '9' }
func cIsAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func cIsAlnum(c byte) bool { return cIsAlpha(c) || cIsDigit(c) }
func cIsPunct(c byte) bool { return c > ' ' && c < 0x7f && !cIsAlnum(c) }

func cAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// cStrtoint is strtoint in base 10: the value, where it stopped (the start where there are no
// digits), and whether it overflowed an int.
func cStrtoint(s string, i int) (int32, int, bool) {
	start := i
	for cIsSpace(cAt(s, i)) {
		i++
	}
	neg := false
	if c := cAt(s, i); c == '+' || c == '-' {
		neg = c == '-'
		i++
	}
	d := i
	var v int64
	over := false
	for cIsDigit(cAt(s, i)) {
		if v < 1<<40 {
			v = v*10 + int64(s[i]-'0')
		} else {
			over = true
		}
		i++
	}
	if i == d {
		return 0, start, false
	}
	if neg {
		v = -v
	}
	if over || v > math.MaxInt32 || v < math.MinInt32 {
		return 0, i, true
	}
	return int32(v), i, false
}

// cStrtod is glibc's strtod in the C locale: the value of the number s[i:] begins with, where it
// stopped (i where there is none), and ERANGE.
func cStrtod(s string, i int) (float64, int, bool) {
	start := i
	for cIsSpace(cAt(s, i)) {
		i++
	}
	j := i
	if c := cAt(s, j); c == '+' || c == '-' {
		j++
	}
	lower := strings.ToLower(s[j:])
	switch {
	case strings.HasPrefix(lower, "infinity"):
		v, _ := strconv.ParseFloat(s[i:j]+"inf", 64)
		return v, j + 8, false
	case strings.HasPrefix(lower, "inf"):
		v, _ := strconv.ParseFloat(s[i:j]+"inf", 64)
		return v, j + 3, false
	case strings.HasPrefix(lower, "nan"):
		k := j + 3
		if cAt(s, k) == '(' {
			m := k + 1
			for cIsAlnum(cAt(s, m)) || cAt(s, m) == '_' {
				m++
			}
			if cAt(s, m) == ')' {
				k = m + 1
			}
		}
		return math.NaN(), k, false
	}
	k := j
	hex := cAt(s, k) == '0' && (cAt(s, k+1) == 'x' || cAt(s, k+1) == 'X')
	isDig := cIsDigit
	if hex {
		isDig = func(c byte) bool { return cIsDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }
		k += 2
	}
	m := k
	for isDig(cAt(s, m)) {
		m++
	}
	nd := m - k
	if cAt(s, m) == '.' {
		m++
		f := m
		for isDig(cAt(s, m)) {
			m++
		}
		nd += m - f
	}
	if nd == 0 {
		if hex {
			// "0x" alone reads as 0, stopping after the 0.
			return 0, j + 1, false
		}
		return 0, start, false
	}
	e := m
	ec, ech := byte('e'), byte('E')
	if hex {
		ec, ech = 'p', 'P'
	}
	if c := cAt(s, e); c == ec || c == ech {
		n := e + 1
		if c := cAt(s, n); c == '+' || c == '-' {
			n++
		}
		if cIsDigit(cAt(s, n)) {
			for cIsDigit(cAt(s, n)) {
				n++
			}
			m = n
		}
	}
	lit := s[i:m]
	if hex && !strings.ContainsAny(lit, "pP") {
		lit += "p0"
	}
	v, err := strconv.ParseFloat(lit, 64)
	// glibc's ERANGE: overflow, or underflow to a subnormal or to zero from a nonzero mantissa.
	erange := err != nil || (v != 0 && math.Abs(v) < 0x1p-1022) || (v == 0 && pgNonzeroMantissa(s[k:m], hex))
	return v, m, erange
}

// pgNonzeroMantissa reports whether a number's digits before its exponent include a nonzero one.
func pgNonzeroMantissa(lit string, hex bool) bool {
	for i := 0; i < len(lit); i++ {
		c := lit[i]
		switch {
		case !hex && (c == 'e' || c == 'E'), hex && (c == 'p' || c == 'P'):
			return false
		case c >= '1' && c <= '9', hex && ((c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')):
			return true
		}
	}
	return false
}

// parseDateTime is ParseDateTime: s split into fields and their types, within a work buffer of
// buflen bytes.
func parseDateTime(s string, buflen int) ([]string, []int, int) {
	var fields []string
	var ftypes []int
	used := 0
	var cur []byte
	appendc := func(c byte) bool {
		if used+1 >= buflen {
			return false
		}
		used++
		cur = append(cur, c)
		return true
	}
	lower := func(c byte) byte {
		if c >= 'A' && c <= 'Z' {
			return c + 'a' - 'A'
		}
		return c
	}
	i := 0
	for i < len(s) && s[i] != 0 {
		c := s[i]
		if cIsSpace(c) {
			i++
			continue
		}
		if len(fields) >= pgMaxDateFields {
			return nil, nil, dtErrBadFormat
		}
		cur = cur[:0]
		var ft int
		switch {
		case cIsDigit(c):
			if !appendc(c) {
				return nil, nil, dtErrBadFormat
			}
			i++
			for cIsDigit(cAt(s, i)) {
				if !appendc(s[i]) {
					return nil, nil, dtErrBadFormat
				}
				i++
			}
			switch cAt(s, i) {
			case ':':
				ft = dtkTime
				if !appendc(s[i]) {
					return nil, nil, dtErrBadFormat
				}
				i++
				for cIsDigit(cAt(s, i)) || cAt(s, i) == ':' || cAt(s, i) == '.' {
					if !appendc(s[i]) {
						return nil, nil, dtErrBadFormat
					}
					i++
				}
			case '-', '/', '.':
				delim := s[i]
				if !appendc(s[i]) {
					return nil, nil, dtErrBadFormat
				}
				i++
				if cIsDigit(cAt(s, i)) {
					if delim == '.' {
						ft = dtkNumber
					} else {
						ft = dtkDate
					}
					for cIsDigit(cAt(s, i)) {
						if !appendc(s[i]) {
							return nil, nil, dtErrBadFormat
						}
						i++
					}
					if cAt(s, i) == delim {
						ft = dtkDate
						if !appendc(s[i]) {
							return nil, nil, dtErrBadFormat
						}
						i++
						for cIsDigit(cAt(s, i)) || cAt(s, i) == delim {
							if !appendc(s[i]) {
								return nil, nil, dtErrBadFormat
							}
							i++
						}
					}
				} else {
					ft = dtkDate
					for cIsAlnum(cAt(s, i)) || cAt(s, i) == delim {
						if !appendc(lower(s[i])) {
							return nil, nil, dtErrBadFormat
						}
						i++
					}
				}
			default:
				ft = dtkNumber
			}
		case c == '.':
			if !appendc(c) {
				return nil, nil, dtErrBadFormat
			}
			i++
			for cIsDigit(cAt(s, i)) {
				if !appendc(s[i]) {
					return nil, nil, dtErrBadFormat
				}
				i++
			}
			ft = dtkNumber
		case cIsAlpha(c):
			ft = dtkString
			if !appendc(lower(c)) {
				return nil, nil, dtErrBadFormat
			}
			i++
			for cIsAlpha(cAt(s, i)) {
				if !appendc(lower(s[i])) {
					return nil, nil, dtErrBadFormat
				}
				i++
			}
			isDate := false
			switch d := cAt(s, i); {
			case d == '-' || d == '/' || d == '.':
				isDate = true
			case d == '+' || cIsDigit(d):
				if _, ok := datebsearch(string(cur), pgDatetktbl); !ok {
					isDate = true
				}
			}
			if isDate {
				ft = dtkDate
				for {
					if !appendc(lower(cAt(s, i))) {
						return nil, nil, dtErrBadFormat
					}
					i++
					d := cAt(s, i)
					if !(d == '+' || d == '-' || d == '/' || d == '_' || d == '.' || d == ':' || cIsAlnum(d)) {
						break
					}
				}
			}
		case c == '+' || c == '-':
			if !appendc(c) {
				return nil, nil, dtErrBadFormat
			}
			i++
			for cIsSpace(cAt(s, i)) {
				i++
			}
			switch d := cAt(s, i); {
			case cIsDigit(d):
				ft = dtkTZ
				if !appendc(d) {
					return nil, nil, dtErrBadFormat
				}
				i++
				for cIsDigit(cAt(s, i)) || cAt(s, i) == ':' || cAt(s, i) == '.' || cAt(s, i) == '-' {
					if !appendc(s[i]) {
						return nil, nil, dtErrBadFormat
					}
					i++
				}
			case cIsAlpha(d):
				ft = dtkSpecial
				if !appendc(lower(d)) {
					return nil, nil, dtErrBadFormat
				}
				i++
				for cIsAlpha(cAt(s, i)) {
					if !appendc(lower(s[i])) {
						return nil, nil, dtErrBadFormat
					}
					i++
				}
			default:
				return nil, nil, dtErrBadFormat
			}
		case cIsPunct(c):
			i++
			continue
		default:
			return nil, nil, dtErrBadFormat
		}
		used++ // the field's terminating NUL
		fields = append(fields, string(cur))
		ftypes = append(ftypes, ft)
	}
	return fields, ftypes, 0
}

// parseFractionalSecond is ParseFractionalSecond over s, which begins with '.'.
func parseFractionalSecond(s string) (int64, int) {
	frac, n, erange := cStrtod(s, 0)
	if n != len(s) || erange {
		return 0, dtErrBadFormat
	}
	return int64(math.RoundToEven(frac * 1000000)), 0
}

// decodeEnv is what decoding reads besides the text: the current time, for now, today and the like.
type decodeEnv struct {
	now      pgTimestamptz
	contract bool // Postgres's own build contracts a*b+c (aarch64)
}

// currentTimeUsec is GetCurrentTimeUsec: the transaction's start in the session zone.
func (e decodeEnv) currentTimeUsec() (pgTm, int64, int32) {
	tm, fsec, tz, _, _ := timestamp2tm(int64(e.now), true, pgSessionZone)
	return tm, fsec, tz
}

// decodeDateTime is DecodeDateTime. tzp is nil for a type without zone.
func decodeDateTime(env decodeEnv, field []string, ftype []int, tm *pgTm, fsec *int64, tzp *int32) (int, int) {
	fmask, tmask := 0, 0
	ptype := 0
	mer := dtHR24
	haveTextMonth, isjulian, is2digits, bc := false, false, false, false
	var namedTz, abbrevTz *pgZone
	var abbrev string
	dtype := dtkDate
	tm.hour, tm.min, tm.sec = 0, 0, 0
	*fsec = 0
	tm.isdst = -1
	if tzp != nil {
		*tzp = 0
	}
	nf := len(field)
	for i := 0; i < nf; i++ {
		f := field[i]
		switch ftype[i] {
		case dtkDate:
			switch {
			case ptype == dtkJulian:
				if tzp == nil {
					return 0, dtErrBadFormat
				}
				val, cp, erange := cStrtoint(f, 0)
				if erange || val < 0 {
					return 0, dtErrFieldOverflow
				}
				tm.year, tm.mon, tm.mday = j2date(val)
				isjulian = true
				if err := decodeTimezone(f[cp:], tzp); err != 0 {
					return 0, err
				}
				tmask = dtkDateM | dtkTimeM | dtkM(tokTZ)
				ptype = 0
			case ptype != 0 || (fmask&(dtkM(tokMonth)|dtkM(tokDay))) == (dtkM(tokMonth)|dtkM(tokDay)):
				if tzp == nil {
					return 0, dtErrBadFormat
				}
				if cIsDigit(cAt(f, 0)) || ptype != 0 {
					if ptype != 0 {
						if ptype != dtkTime {
							return 0, dtErrBadFormat
						}
						ptype = 0
					}
					if (fmask & dtkTimeM) == dtkTimeM {
						return 0, dtErrBadFormat
					}
					cp := strings.IndexByte(f, '-')
					if cp < 0 {
						return 0, dtErrBadFormat
					}
					if err := decodeTimezone(f[cp:], tzp); err != 0 {
						return 0, err
					}
					var err int
					tmask, err = decodeNumberField(f[:cp], fmask, tm, fsec, &is2digits)
					if err < 0 {
						return 0, err
					}
					tmask |= dtkM(tokTZ)
				} else {
					z, ok := pgTzset(f)
					if !ok {
						tm.zone = f
						return 0, pgDTErrTZNotRecognized
					}
					namedTz = &z
					tmask = dtkM(tokTZ)
				}
			default:
				var err int
				tmask, err = decodeDate(f, fmask, &is2digits, tm)
				if err != 0 {
					return 0, err
				}
			}
		case dtkTime:
			if ptype != 0 {
				if ptype != dtkTime {
					return 0, dtErrBadFormat
				}
				ptype = 0
			}
			var err int
			tmask, err = decodeTime(f, pgIntervalFullRange, tm, fsec)
			if err != 0 {
				return 0, err
			}
			if pgTimeOverflows(tm.hour, tm.min, tm.sec, *fsec) {
				return 0, dtErrFieldOverflow
			}
		case dtkTZ:
			if tzp == nil {
				return 0, dtErrBadFormat
			}
			var tz int32
			if err := decodeTimezone(f, &tz); err != 0 {
				return 0, err
			}
			*tzp = tz
			tmask = dtkM(tokTZ)
		case dtkNumber:
			if ptype != 0 {
				val, cp, erange := cStrtoint(f, 0)
				if erange {
					return 0, dtErrFieldOverflow
				}
				if cAt(f, cp) == '.' {
					switch ptype {
					case dtkJulian, dtkTime, dtkSecond:
					default:
						return 0, dtErrBadFormat
					}
				} else if cp != len(f) {
					return 0, dtErrBadFormat
				}
				switch ptype {
				case dtkYear:
					tm.year = val
					tmask = dtkM(tokYear)
				case dtkMonth:
					if fmask&dtkM(tokMonth) != 0 && fmask&dtkM(tokHour) != 0 {
						tm.min = val
						tmask = dtkM(tokMinute)
					} else {
						tm.mon = val
						tmask = dtkM(tokMonth)
					}
				case dtkDay:
					tm.mday = val
					tmask = dtkM(tokDay)
				case dtkHour:
					tm.hour = val
					tmask = dtkM(tokHour)
				case dtkMinute:
					tm.min = val
					tmask = dtkM(tokMinute)
				case dtkSecond:
					tm.sec = val
					tmask = dtkM(tokSecond)
					if cAt(f, cp) == '.' {
						fs, err := parseFractionalSecond(f[cp:])
						if err != 0 {
							return 0, err
						}
						*fsec = fs
						tmask = dtkAllSecsM
					}
				case dtkTZ:
					tmask = dtkM(tokTZ)
					if err := decodeTimezone(f, tzp); err != 0 {
						return 0, err
					}
				case dtkJulian:
					if val < 0 {
						return 0, dtErrFieldOverflow
					}
					tmask = dtkDateM
					tm.year, tm.mon, tm.mday = j2date(val)
					isjulian = true
					if cAt(f, cp) == '.' {
						t, n, erange := cStrtod(f, cp)
						if n != len(f) || erange {
							return 0, dtErrBadFormat
						}
						t *= float64(pgUsecsPerDay)
						tm.hour, tm.min, tm.sec, *fsec = dt2time(int64(t))
						tmask |= dtkTimeM
					}
				case dtkTime:
					var err int
					tmask, err = decodeNumberField(f, fmask|dtkDateM, tm, fsec, &is2digits)
					if err < 0 {
						return 0, err
					}
					if tmask != dtkTimeM {
						return 0, dtErrBadFormat
					}
				default:
					return 0, dtErrBadFormat
				}
				ptype = 0
				dtype = dtkDate
			} else {
				flen := len(f)
				cp := strings.IndexByte(f, '.')
				var err int
				switch {
				case cp >= 0 && fmask&dtkDateM == 0:
					tmask, err = decodeDate(f, fmask, &is2digits, tm)
					if err != 0 {
						return 0, err
					}
				case cp >= 0 && cp > 2:
					tmask, err = decodeNumberField(f, fmask, tm, fsec, &is2digits)
					if err < 0 {
						return 0, err
					}
				case flen >= 6 && (fmask&dtkDateM == 0 || fmask&dtkTimeM == 0):
					tmask, err = decodeNumberField(f, fmask, tm, fsec, &is2digits)
					if err < 0 {
						return 0, err
					}
				default:
					tmask, err = decodeNumber(f, haveTextMonth, fmask, tm, fsec, &is2digits)
					if err != 0 {
						return 0, err
					}
				}
			}
		case dtkString, dtkSpecial:
			typ, val, valtz, _ := decodeTimezoneAbbrev(f)
			if typ == tokUnknown {
				typ, val = decodeSpecial(f)
			}
			if typ == tokIgnore {
				continue
			}
			tmask = dtkM(typ)
			switch typ {
			case tokReserv:
				switch val {
				case dtkNow:
					tmask = dtkDateM | dtkTimeM | dtkM(tokTZ)
					dtype = dtkDate
					ctm, cfsec, ctz := env.currentTimeUsec()
					*tm = ctm
					*fsec = cfsec
					if tzp != nil {
						*tzp = ctz
					}
				case dtkYesterday, dtkToday, dtkTomorrow:
					tmask = dtkDateM
					dtype = dtkDate
					cur, _, _ := env.currentTimeUsec()
					j := date2j(cur.year, cur.mon, cur.mday)
					switch val {
					case dtkYesterday:
						j--
					case dtkTomorrow:
						j++
					}
					tm.year, tm.mon, tm.mday = j2date(j)
				case dtkZulu:
					tmask = dtkTimeM | dtkM(tokTZ)
					dtype = dtkDate
					tm.hour, tm.min, tm.sec = 0, 0, 0
					if tzp != nil {
						*tzp = 0
					}
				default:
					dtype = int(val)
				}
			case tokMonth:
				if fmask&dtkM(tokMonth) != 0 && !haveTextMonth && fmask&dtkM(tokDay) == 0 && tm.mon >= 1 && tm.mon <= 31 {
					tm.mday = tm.mon
					tmask = dtkM(tokDay)
				}
				haveTextMonth = true
				tm.mon = val
			case tokDTZMod:
				tmask |= dtkM(tokDTZ)
				tm.isdst = 1
				if tzp == nil {
					return 0, dtErrBadFormat
				}
				*tzp -= val
			case tokDTZ:
				tmask |= dtkM(tokTZ)
				tm.isdst = 1
				if tzp == nil {
					return 0, dtErrBadFormat
				}
				*tzp = -val
			case tokTZ:
				tm.isdst = 0
				if tzp == nil {
					return 0, dtErrBadFormat
				}
				*tzp = -val
			case tokDynTZ:
				tmask |= dtkM(tokTZ)
				if tzp == nil {
					return 0, dtErrBadFormat
				}
				z := valtz
				abbrevTz = &z
				abbrev = f
			case tokAMPM:
				mer = int(val)
			case tokADBC:
				bc = val == dtBC
			case tokDOW:
				tm.wday = val
			case tokUnits:
				tmask = 0
				ptype = int(val)
			case tokISOTime:
				tmask = 0
				if fmask&dtkDateM != dtkDateM {
					return 0, dtErrBadFormat
				}
				if i >= nf-1 || (ftype[i+1] != dtkNumber && ftype[i+1] != dtkTime && ftype[i+1] != dtkDate) {
					return 0, dtErrBadFormat
				}
				ptype = int(val)
			case tokUnknown:
				z, ok := pgTzset(f)
				if !ok {
					return 0, dtErrBadFormat
				}
				namedTz = &z
				tmask = dtkM(tokTZ)
			default:
				return 0, dtErrBadFormat
			}
		default:
			return 0, dtErrBadFormat
		}
		if tmask&fmask != 0 {
			return 0, dtErrBadFormat
		}
		fmask |= tmask
	}
	if err := validateDate(fmask, isjulian, is2digits, bc, tm); err != 0 {
		return 0, err
	}
	if mer != dtHR24 && tm.hour > pgHoursPerDay/2 {
		return 0, dtErrFieldOverflow
	}
	if mer == dtAM && tm.hour == pgHoursPerDay/2 {
		tm.hour = 0
	} else if mer == dtPM && tm.hour != pgHoursPerDay/2 {
		tm.hour += pgHoursPerDay / 2
	}
	if dtype == dtkDate {
		if fmask&dtkDateM != dtkDateM {
			if fmask&dtkTimeM == dtkTimeM {
				return dtype, 1
			}
			return 0, dtErrBadFormat
		}
		if namedTz != nil {
			if fmask&dtkM(tokDTZMod) != 0 {
				return 0, dtErrBadFormat
			}
			*tzp, _ = determineTimeZoneOffset(tm, *namedTz)
		}
		if abbrevTz != nil {
			if fmask&dtkM(tokDTZMod) != 0 {
				return 0, dtErrBadFormat
			}
			*tzp = determineTimeZoneAbbrevOffset(tm, abbrev, *abbrevTz)
		}
		if tzp != nil && fmask&dtkM(tokTZ) == 0 {
			if fmask&dtkM(tokDTZMod) != 0 {
				return 0, dtErrBadFormat
			}
			*tzp, _ = determineTimeZoneOffset(tm, pgSessionZone)
		}
	}
	return dtype, 0
}

// pgDTErrTZNotRecognized is the "time zone not recognized" error DecodeDateTime raises itself.
const pgDTErrTZNotRecognized = -100

// pgTimeOverflows is time_overflows.
func pgTimeOverflows(hour, min, sec int32, fsec int64) bool {
	if hour < 0 || hour > pgHoursPerDay || min < 0 || min >= pgMinsPerHour || sec < 0 || sec > pgSecsPerMinute ||
		fsec < 0 || fsec > pgUsecsPerSec {
		return true
	}
	return ((((int64(hour)*pgMinsPerHour+int64(min))*pgSecsPerMinute)+int64(sec))*pgUsecsPerSec)+fsec > pgUsecsPerDay
}

// decodeDate is DecodeDate.
func decodeDate(str string, fmask int, is2digits *bool, tm *pgTm) (int, int) {
	tmask := 0
	var field []string
	i := 0
	for i < len(str) && len(field) < pgMaxDateFields {
		for i < len(str) && !cIsAlnum(str[i]) {
			i++
		}
		if i >= len(str) {
			return 0, dtErrBadFormat
		}
		start := i
		if cIsDigit(str[i]) {
			for i < len(str) && cIsDigit(str[i]) {
				i++
			}
		} else if cIsAlpha(str[i]) {
			for i < len(str) && cIsAlpha(str[i]) {
				i++
			}
		}
		field = append(field, str[start:i])
		if i < len(str) {
			i++ // the separator, overwritten with NUL
		}
	}
	haveTextMonth := false
	used := make([]bool, len(field))
	for k, f := range field {
		if !cIsAlpha(cAt(f, 0)) {
			continue
		}
		typ, val := decodeSpecial(f)
		if typ == tokIgnore {
			continue
		}
		dmask := dtkM(typ)
		switch typ {
		case tokMonth:
			tm.mon = val
			haveTextMonth = true
		default:
			return 0, dtErrBadFormat
		}
		if fmask&dmask != 0 {
			return 0, dtErrBadFormat
		}
		fmask |= dmask
		tmask |= dmask
		used[k] = true
	}
	for k, f := range field {
		if used[k] {
			continue
		}
		if len(f) <= 0 {
			return 0, dtErrBadFormat
		}
		var fsec int64
		dmask, err := decodeNumber(f, haveTextMonth, fmask, tm, &fsec, is2digits)
		if err != 0 {
			return 0, err
		}
		if fmask&dmask != 0 {
			return 0, dtErrBadFormat
		}
		fmask |= dmask
		tmask |= dmask
	}
	if fmask&^(dtkM(tokDOY)|dtkM(tokTZ)) != dtkDateM {
		return 0, dtErrBadFormat
	}
	return tmask, 0
}

// validateDate is ValidateDate.
func validateDate(fmask int, isjulian, is2digits, bc bool, tm *pgTm) int {
	if fmask&dtkM(tokYear) != 0 {
		switch {
		case isjulian:
		case bc:
			if tm.year <= 0 {
				return dtErrFieldOverflow
			}
			tm.year = -(tm.year - 1)
		case is2digits:
			if tm.year < 0 {
				return dtErrFieldOverflow
			}
			if tm.year < 70 {
				tm.year += 2000
			} else if tm.year < 100 {
				tm.year += 1900
			}
		default:
			if tm.year <= 0 {
				return dtErrFieldOverflow
			}
		}
	}
	if fmask&dtkM(tokDOY) != 0 {
		tm.year, tm.mon, tm.mday = j2date(date2j(tm.year, 1, 1) + tm.yday - 1)
	}
	if fmask&dtkM(tokMonth) != 0 {
		if tm.mon < 1 || tm.mon > pgMonthsPerYear {
			return dtErrMDFieldOverflow
		}
	}
	if fmask&dtkM(tokDay) != 0 {
		if tm.mday < 1 || tm.mday > 31 {
			return dtErrMDFieldOverflow
		}
	}
	if fmask&dtkDateM == dtkDateM {
		if tm.mday > pgDayTab[pgIsLeap(tm.year)][tm.mon-1] {
			return dtErrFieldOverflow
		}
	}
	return 0
}

// decodeTime is DecodeTime.
func decodeTime(str string, rng int, tm *pgTm, fsec *int64) (int, int) {
	tmask := dtkTimeM
	h, cp, erange := cStrtoint(str, 0)
	if erange {
		return 0, dtErrFieldOverflow
	}
	tm.hour = h
	if cAt(str, cp) != ':' {
		return 0, dtErrBadFormat
	}
	m, cp2, erange := cStrtoint(str, cp+1)
	if erange {
		return 0, dtErrFieldOverflow
	}
	tm.min = m
	cp = cp2
	switch cAt(str, cp) {
	case 0:
		tm.sec = 0
		*fsec = 0
		if rng == (1<<tokMinute | 1<<tokSecond) {
			tm.sec = tm.min
			tm.min = tm.hour
			tm.hour = 0
		}
	case '.':
		fs, err := parseFractionalSecond(str[cp:])
		if err != 0 {
			return 0, err
		}
		*fsec = fs
		tm.sec = tm.min
		tm.min = tm.hour
		tm.hour = 0
	case ':':
		s, cp3, erange := cStrtoint(str, cp+1)
		if erange {
			return 0, dtErrFieldOverflow
		}
		tm.sec = s
		cp = cp3
		switch cAt(str, cp) {
		case 0:
			*fsec = 0
		case '.':
			fs, err := parseFractionalSecond(str[cp:])
			if err != 0 {
				return 0, err
			}
			*fsec = fs
		default:
			return 0, dtErrBadFormat
		}
	default:
		return 0, dtErrBadFormat
	}
	if tm.hour < 0 || tm.min < 0 || tm.min > pgMinsPerHour-1 || tm.sec < 0 || tm.sec > pgSecsPerMinute ||
		*fsec < 0 || *fsec > pgUsecsPerSec {
		return 0, dtErrFieldOverflow
	}
	return tmask, 0
}

// decodeNumber is DecodeNumber, in DateOrder MDY.
func decodeNumber(str string, haveTextMonth bool, fmask int, tm *pgTm, fsec *int64, is2digits *bool) (int, int) {
	flen := len(str)
	tmask := 0
	val, cp, erange := cStrtoint(str, 0)
	if erange {
		return 0, dtErrFieldOverflow
	}
	if cp == 0 {
		return 0, dtErrBadFormat
	}
	if cAt(str, cp) == '.' {
		if cp > 2 {
			tm2, err := decodeNumberField(str, fmask|dtkDateM, tm, fsec, is2digits)
			if err < 0 {
				return 0, err
			}
			return tm2, 0
		}
		fs, err := parseFractionalSecond(str[cp:])
		if err != 0 {
			return 0, err
		}
		*fsec = fs
	} else if cp != len(str) {
		return 0, dtErrBadFormat
	}
	if flen == 3 && fmask&dtkDateM == dtkM(tokYear) && val >= 1 && val <= 366 {
		tm.yday = val
		return dtkM(tokDOY) | dtkM(tokMonth) | dtkM(tokDay), 0
	}
	switch fmask & dtkDateM {
	case 0:
		if flen >= 3 {
			tmask = dtkM(tokYear)
			tm.year = val
		} else {
			tmask = dtkM(tokMonth)
			tm.mon = val
		}
	case dtkM(tokYear):
		tmask = dtkM(tokMonth)
		tm.mon = val
	case dtkM(tokMonth):
		if haveTextMonth {
			if flen >= 3 {
				tmask = dtkM(tokYear)
				tm.year = val
			} else {
				tmask = dtkM(tokDay)
				tm.mday = val
			}
		} else {
			tmask = dtkM(tokDay)
			tm.mday = val
		}
	case dtkM(tokYear) | dtkM(tokMonth):
		if haveTextMonth {
			if flen >= 3 && *is2digits {
				tmask = dtkM(tokDay)
				tm.mday = tm.year
				tm.year = val
				*is2digits = false
			} else {
				tmask = dtkM(tokDay)
				tm.mday = val
			}
		} else {
			tmask = dtkM(tokDay)
			tm.mday = val
		}
	case dtkM(tokDay):
		tmask = dtkM(tokMonth)
		tm.mon = val
	case dtkM(tokMonth) | dtkM(tokDay):
		tmask = dtkM(tokYear)
		tm.year = val
	case dtkDateM:
		tm2, err := decodeNumberField(str, fmask, tm, fsec, is2digits)
		if err < 0 {
			return 0, err
		}
		return tm2, 0
	default:
		return 0, dtErrBadFormat
	}
	if tmask == dtkM(tokYear) {
		*is2digits = flen <= 2
	}
	return tmask, 0
}

// cAtoi is atoi: the leading integer, as strtol reads it truncated to int.
func cAtoi(s string) int32 {
	v, _, _ := cStrtoint(s, 0)
	return v
}

// decodeNumberField is DecodeNumberField: a run of digits as a concatenated date or time.
func decodeNumberField(str string, fmask int, tm *pgTm, fsec *int64, is2digits *bool) (int, int) {
	if cp := strings.IndexByte(str, '.'); cp >= 0 {
		frac, _, erange := cStrtod(str, cp)
		if erange {
			return 0, dtErrBadFormat
		}
		*fsec = int64(math.RoundToEven(frac * 1000000))
		str = str[:cp]
	} else if fmask&dtkDateM != dtkDateM {
		if n := len(str); n >= 6 {
			tm.mday = cAtoi(str[n-2:])
			tm.mon = cAtoi(str[n-4 : n-2])
			tm.year = cAtoi(str[:n-4])
			if n-4 == 2 {
				*is2digits = true
			}
			return dtkDateM, dtkDate
		}
	}
	n := len(str)
	if fmask&dtkTimeM != dtkTimeM {
		if n == 6 {
			tm.sec = cAtoi(str[4:])
			tm.min = cAtoi(str[2:4])
			tm.hour = cAtoi(str[:2])
			return dtkTimeM, dtkTime
		} else if n == 4 {
			tm.sec = 0
			tm.min = cAtoi(str[2:])
			tm.hour = cAtoi(str[:2])
			return dtkTimeM, dtkTime
		}
	}
	return 0, dtErrBadFormat
}

// decodeTimezone is DecodeTimezone: +hh[:mm[:ss]] or +hhmm, into tz seconds west.
func decodeTimezone(str string, tzp *int32) int {
	c := cAt(str, 0)
	if c != '+' && c != '-' {
		return dtErrBadFormat
	}
	hr, cp, erange := cStrtoint(str, 1)
	if erange {
		return dtErrTZDispOverflow
	}
	var min, sec int32
	switch {
	case cAt(str, cp) == ':':
		min, cp, erange = cStrtoint(str, cp+1)
		if erange {
			return dtErrTZDispOverflow
		}
		if cAt(str, cp) == ':' {
			sec, cp, erange = cStrtoint(str, cp+1)
			if erange {
				return dtErrTZDispOverflow
			}
		}
	case cp == len(str) && len(str) > 3:
		min = hr % 100
		hr = hr / 100
	default:
		min = 0
	}
	if hr < 0 || hr > pgMaxTZDispHour {
		return dtErrTZDispOverflow
	}
	if min < 0 || min >= pgMinsPerHour {
		return dtErrTZDispOverflow
	}
	if sec < 0 || sec >= pgSecsPerMinute {
		return dtErrTZDispOverflow
	}
	tz := (hr*pgMinsPerHour+min)*pgSecsPerMinute + sec
	if c == '-' {
		tz = -tz
	}
	*tzp = -tz
	if cp != len(str) {
		return dtErrBadFormat
	}
	return 0
}
