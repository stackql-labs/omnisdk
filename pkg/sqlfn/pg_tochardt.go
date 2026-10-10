package sqlfn

import (
	"fmt"
	"math"
	"strings"
)

// to_char over timestamps and intervals, and to_timestamp, ported from Postgres 14's formatting.c
// (the DCH_* half), as stackql's Postgres runs them: lc_time C, so English names, which TM leaves
// unpadded; the C collation, so case is ASCII's; session TimeZone Etc/UTC.

const (
	dchModeNone = iota
	dchModeGregorian
	dchModeISOWeek
)

// dchKeyword is a KeyWord of DCH_keywords.
type dchKeyword struct {
	name    string
	id      int
	isDigit bool
	mode    int
}

const (
	dchA_D = iota
	dchA_M
	dchAD
	dchAM
	dchB_C
	dchBC
	dchCC
	dchDAY
	dchDDD
	dchDD
	dchDY
	dchDay
	dchDy
	dchD
	dchFF1
	dchFF2
	dchFF3
	dchFF4
	dchFF5
	dchFF6
	dchFX
	dchHH24
	dchHH12
	dchHH
	dchIDDD
	dchID
	dchIW
	dchIYYY
	dchIYY
	dchIY
	dchI
	dchJ
	dchMI
	dchMM
	dchMONTH
	dchMON
	dchMS
	dchMonth
	dchMon
	dchOF
	dchP_M
	dchPM
	dchQ
	dchRM
	dchSSSSS
	dchSSSS
	dchSS
	dchTZH
	dchTZM
	dchTZ
	dchUS
	dchWW
	dchW
	dchY_YYY
	dchYYYY
	dchYYY
	dchYY
	dchY
	dcha_d
	dcha_m
	dchad
	dcham
	dchb_c
	dchbc
	dchcc
	dchday
	dchddd
	dchdd
	dchdy
	dchd
	dchff1
	dchff2
	dchff3
	dchff4
	dchff5
	dchff6
	dchfx
	dchhh24
	dchhh12
	dchhh
	dchiddd
	dchid
	dchiw
	dchiyyy
	dchiyy
	dchiy
	dchi
	dchj
	dchmi
	dchmm
	dchmonth
	dchmon
	dchms
	dchp_m
	dchpm
	dchq
	dchrm
	dchsssss
	dchssss
	dchss
	dchtz
	dchus
	dchww
	dchw
	dchy_yyy
	dchyyyy
	dchyyy
	dchyy
	dchy
)

// dchKeywords is DCH_keywords, in its search order.
var dchKeywords = []dchKeyword{
	{"A.D.", dchA_D, false, dchModeNone},
	{"A.M.", dchA_M, false, dchModeNone},
	{"AD", dchAD, false, dchModeNone},
	{"AM", dchAM, false, dchModeNone},
	{"B.C.", dchB_C, false, dchModeNone},
	{"BC", dchBC, false, dchModeNone},
	{"CC", dchCC, true, dchModeNone},
	{"DAY", dchDAY, false, dchModeNone},
	{"DDD", dchDDD, true, dchModeGregorian},
	{"DD", dchDD, true, dchModeGregorian},
	{"DY", dchDY, false, dchModeNone},
	{"Day", dchDay, false, dchModeNone},
	{"Dy", dchDy, false, dchModeNone},
	{"D", dchD, true, dchModeGregorian},
	{"FF1", dchFF1, false, dchModeNone},
	{"FF2", dchFF2, false, dchModeNone},
	{"FF3", dchFF3, false, dchModeNone},
	{"FF4", dchFF4, false, dchModeNone},
	{"FF5", dchFF5, false, dchModeNone},
	{"FF6", dchFF6, false, dchModeNone},
	{"FX", dchFX, false, dchModeNone},
	{"HH24", dchHH24, true, dchModeNone},
	{"HH12", dchHH12, true, dchModeNone},
	{"HH", dchHH, true, dchModeNone},
	{"IDDD", dchIDDD, true, dchModeISOWeek},
	{"ID", dchID, true, dchModeISOWeek},
	{"IW", dchIW, true, dchModeISOWeek},
	{"IYYY", dchIYYY, true, dchModeISOWeek},
	{"IYY", dchIYY, true, dchModeISOWeek},
	{"IY", dchIY, true, dchModeISOWeek},
	{"I", dchI, true, dchModeISOWeek},
	{"J", dchJ, true, dchModeNone},
	{"MI", dchMI, true, dchModeNone},
	{"MM", dchMM, true, dchModeGregorian},
	{"MONTH", dchMONTH, false, dchModeGregorian},
	{"MON", dchMON, false, dchModeGregorian},
	{"MS", dchMS, true, dchModeNone},
	{"Month", dchMonth, false, dchModeGregorian},
	{"Mon", dchMon, false, dchModeGregorian},
	{"OF", dchOF, false, dchModeNone},
	{"P.M.", dchP_M, false, dchModeNone},
	{"PM", dchPM, false, dchModeNone},
	{"Q", dchQ, true, dchModeNone},
	{"RM", dchRM, false, dchModeGregorian},
	{"SSSSS", dchSSSS, true, dchModeNone},
	{"SSSS", dchSSSS, true, dchModeNone},
	{"SS", dchSS, true, dchModeNone},
	{"TZH", dchTZH, false, dchModeNone},
	{"TZM", dchTZM, true, dchModeNone},
	{"TZ", dchTZ, false, dchModeNone},
	{"US", dchUS, true, dchModeNone},
	{"WW", dchWW, true, dchModeGregorian},
	{"W", dchW, true, dchModeGregorian},
	{"Y,YYY", dchY_YYY, true, dchModeGregorian},
	{"YYYY", dchYYYY, true, dchModeGregorian},
	{"YYY", dchYYY, true, dchModeGregorian},
	{"YY", dchYY, true, dchModeGregorian},
	{"Y", dchY, true, dchModeGregorian},
	{"a.d.", dcha_d, false, dchModeNone},
	{"a.m.", dcha_m, false, dchModeNone},
	{"ad", dchad, false, dchModeNone},
	{"am", dcham, false, dchModeNone},
	{"b.c.", dchb_c, false, dchModeNone},
	{"bc", dchbc, false, dchModeNone},
	{"cc", dchCC, true, dchModeNone},
	{"day", dchday, false, dchModeNone},
	{"ddd", dchDDD, true, dchModeGregorian},
	{"dd", dchDD, true, dchModeGregorian},
	{"dy", dchdy, false, dchModeNone},
	{"d", dchD, true, dchModeGregorian},
	{"ff1", dchFF1, false, dchModeNone},
	{"ff2", dchFF2, false, dchModeNone},
	{"ff3", dchFF3, false, dchModeNone},
	{"ff4", dchFF4, false, dchModeNone},
	{"ff5", dchFF5, false, dchModeNone},
	{"ff6", dchFF6, false, dchModeNone},
	{"fx", dchFX, false, dchModeNone},
	{"hh24", dchHH24, true, dchModeNone},
	{"hh12", dchHH12, true, dchModeNone},
	{"hh", dchHH, true, dchModeNone},
	{"iddd", dchIDDD, true, dchModeISOWeek},
	{"id", dchID, true, dchModeISOWeek},
	{"iw", dchIW, true, dchModeISOWeek},
	{"iyyy", dchIYYY, true, dchModeISOWeek},
	{"iyy", dchIYY, true, dchModeISOWeek},
	{"iy", dchIY, true, dchModeISOWeek},
	{"i", dchI, true, dchModeISOWeek},
	{"j", dchJ, true, dchModeNone},
	{"mi", dchMI, true, dchModeNone},
	{"mm", dchMM, true, dchModeGregorian},
	{"month", dchmonth, false, dchModeGregorian},
	{"mon", dchmon, false, dchModeGregorian},
	{"ms", dchMS, true, dchModeNone},
	{"p.m.", dchp_m, false, dchModeNone},
	{"pm", dchpm, false, dchModeNone},
	{"q", dchQ, true, dchModeNone},
	{"rm", dchrm, false, dchModeGregorian},
	{"sssss", dchSSSS, true, dchModeNone},
	{"ssss", dchSSSS, true, dchModeNone},
	{"ss", dchSS, true, dchModeNone},
	{"tz", dchtz, false, dchModeNone},
	{"us", dchUS, true, dchModeNone},
	{"ww", dchWW, true, dchModeGregorian},
	{"w", dchW, true, dchModeGregorian},
	{"y,yyy", dchY_YYY, true, dchModeGregorian},
	{"yyyy", dchYYYY, true, dchModeGregorian},
	{"yyy", dchYYY, true, dchModeGregorian},
	{"yy", dchYY, true, dchModeGregorian},
	{"y", dchY, true, dchModeGregorian},
}

const (
	dchSuffFM = 0x01
	dchSuffTH = 0x02
	dchSuffth = 0x04
	dchSuffSP = 0x08
	dchSuffTM = 0x10

	dchNodeAction = iota
	dchNodeChar
	dchNodeSeparator
	dchNodeSpace
)

// dchSuffixes is DCH_suff.
var dchSuffixes = []struct {
	name    string
	id      int
	postfix bool
}{
	{"FM", dchSuffFM, false}, {"fm", dchSuffFM, false}, {"TM", dchSuffTM, false}, {"tm", dchSuffTM, false},
	{"TH", dchSuffTH, true}, {"th", dchSuffth, true}, {"SP", dchSuffSP, true},
}

// dchNode is a FormatNode.
type dchNode struct {
	typ    int
	chr    string
	suffix int
	key    *dchKeyword
}

func (n dchNode) fm() bool   { return n.suffix&dchSuffFM != 0 }
func (n dchNode) thth() bool { return n.suffix&(dchSuffTH|dchSuffth) != 0 }
func (n dchNode) tm() bool   { return n.suffix&dchSuffTM != 0 }

// dchSuffSearch is suff_search.
func dchSuffSearch(s string, postfix bool) (string, int, bool) {
	for _, x := range dchSuffixes {
		if x.postfix == postfix && strings.HasPrefix(s, x.name) {
			return x.name, x.id, true
		}
	}
	return "", 0, false
}

// dchKeywordSearch is index_seq_search over DCH_keywords.
func dchKeywordSearch(s string) *dchKeyword {
	for i := range dchKeywords {
		if strings.HasPrefix(s, dchKeywords[i].name) {
			return &dchKeywords[i]
		}
	}
	return nil
}

// dchIsSeparatorChar is is_separator_char.
func dchIsSeparatorChar(c byte) bool {
	return c > 0x20 && c < 0x7F && !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9')
}

// dchParse is parse_format with DCH_FLAG.
func dchParse(str string) []dchNode {
	var nodes []dchNode
	for len(str) > 0 {
		suffix := 0
		if name, id, ok := dchSuffSearch(str, false); ok {
			suffix |= id
			str = str[len(name):]
		}
		if len(str) == 0 {
			break
		}
		if k := dchKeywordSearch(str); k != nil {
			str = str[len(k.name):]
			n := dchNode{typ: dchNodeAction, suffix: suffix, key: k}
			if name, id, ok := dchSuffSearch(str, true); ok && len(str) > 0 {
				n.suffix |= id
				str = str[len(name):]
			}
			nodes = append(nodes, n)
			continue
		}
		if str[0] == '"' {
			str = str[1:]
			for len(str) > 0 {
				if str[0] == '"' {
					str = str[1:]
					break
				}
				if str[0] == '\\' && len(str) > 1 {
					str = str[1:]
				}
				l := pgNumMblen(str)
				nodes = append(nodes, dchNode{typ: dchNodeChar, chr: str[:l]})
				str = str[l:]
			}
			continue
		}
		if str[0] == '\\' && len(str) > 1 && str[1] == '"' {
			str = str[1:]
		}
		l := pgNumMblen(str)
		typ := dchNodeChar
		if dchIsSeparatorChar(str[0]) {
			typ = dchNodeSeparator
		} else if cIsSpace(str[0]) {
			typ = dchNodeSpace
		}
		nodes = append(nodes, dchNode{typ: typ, chr: str[:l]})
		str = str[l:]
	}
	return nodes
}

var (
	dchMonthsFull   = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	dchDaysShort    = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	dchRMUpper      = []string{"XII", "XI", "X", "IX", "VIII", "VII", "VI", "V", "IV", "III", "II", "I"}
	dchRMLower      = []string{"xii", "xi", "x", "ix", "viii", "vii", "vi", "v", "iv", "iii", "ii", "i"}
	dchAmPm         = []string{"am", "pm", "AM", "PM"}
	dchAmPmLong     = []string{"a.m.", "p.m.", "A.M.", "P.M."}
	dchAdBc         = []string{"ad", "bc", "AD", "BC"}
	dchAdBcLong     = []string{"a.d.", "b.c.", "A.D.", "B.C."}
	dchErrInterval  = pgErrorf("invalid format specification for an interval value")
	dchErrTimestamp = pgErrorf("timestamp out of range")
)

// dchAdjustYear is ADJUST_YEAR.
func dchAdjustYear(year int32, isInterval bool) int32 {
	if isInterval || year > 0 {
		return year
	}
	return -(year - 1)
}

// dchDate2isoyearday is date2isoyearday.
func dchDate2isoyearday(year, mon, mday int32) int32 {
	return date2j(year, mon, mday) - isoweek2j(date2isoyear(year, mon, mday), 1) + 1
}

// dchToChar is DCH_to_char: tzn is nil where the value has no zone.
func dchToChar(nodes []dchNode, isInterval bool, tm pgTm, fsec int64, tzn *string) (string, error) {
	var b strings.Builder
	for _, n := range nodes {
		if n.typ != dchNodeAction {
			b.WriteString(n.chr)
			continue
		}
		var s string
		num := false
		width := func(w, wneg int, v int32) int {
			if n.fm() {
				return 0
			}
			if v >= 0 {
				return w
			}
			return wneg
		}
		pad := func(w int, v int32) { s, num = fmt.Sprintf("%0*d", w, v), true }
		name := func(full string, upper, lower bool, padded bool) {
			switch {
			case upper:
				full = strings.ToUpper(full)
			case lower:
				full = strings.ToLower(full)
			}
			if padded && !n.tm() && !n.fm() {
				full = fmt.Sprintf("%-9s", full)
			}
			s = full
		}
		pm := tm.hour%pgHoursPerDay >= pgHoursPerDay/2
		id := n.key.id
		switch id {
		case dchTZ, dchtz, dchTZH, dchTZM, dchOF, dchA_D, dchB_C, dchAD, dchBC, dcha_d, dchb_c, dchad, dchbc,
			dchMONTH, dchMonth, dchmonth, dchMON, dchMon, dchmon, dchDAY, dchDay, dchday, dchDY, dchDy, dchdy,
			dchD, dchID:
			if isInterval {
				return "", dchErrInterval
			}
		}
		yearOf := func(iso bool) int32 {
			if iso {
				return dchAdjustYear(date2isoyear(tm.year, tm.mon, tm.mday), isInterval)
			}
			return dchAdjustYear(tm.year, isInterval)
		}
		switch id {
		case dchA_M, dchP_M:
			s = map[bool]string{true: "P.M.", false: "A.M."}[pm]
		case dchAM, dchPM:
			s = map[bool]string{true: "PM", false: "AM"}[pm]
		case dcha_m, dchp_m:
			s = map[bool]string{true: "p.m.", false: "a.m."}[pm]
		case dcham, dchpm:
			s = map[bool]string{true: "pm", false: "am"}[pm]
		case dchHH, dchHH12:
			h := tm.hour % (pgHoursPerDay / 2)
			if h == 0 {
				h = pgHoursPerDay / 2
			}
			pad(width(2, 3, tm.hour), h)
		case dchHH24:
			pad(width(2, 3, tm.hour), tm.hour)
		case dchMI:
			pad(width(2, 3, tm.min), tm.min)
		case dchSS:
			pad(width(2, 3, tm.sec), tm.sec)
		case dchFF1:
			pad(1, int32(fsec/100000))
		case dchFF2:
			pad(2, int32(fsec/10000))
		case dchFF3, dchMS:
			pad(3, int32(fsec/1000))
		case dchFF4:
			pad(4, int32(fsec/100))
		case dchFF5:
			pad(5, int32(fsec/10))
		case dchFF6, dchUS:
			pad(6, int32(fsec))
		case dchSSSS:
			pad(0, tm.hour*pgSecsPerHour+tm.min*pgSecsPerMinute+tm.sec)
		case dchtz:
			if tzn != nil {
				s = strings.ToLower(*tzn)
			}
		case dchTZ:
			if tzn != nil {
				s = *tzn
			}
		case dchTZH:
			s = fmt.Sprintf("%c%02d", dchSignChar(tm.gmtoff), dchAbsInt(tm.gmtoff)/pgSecsPerHour)
		case dchTZM:
			s = fmt.Sprintf("%02d", dchAbsInt(tm.gmtoff)%pgSecsPerHour/pgSecsPerMinute)
		case dchOF:
			w := 2
			if n.fm() {
				w = 0
			}
			a := dchAbsInt(tm.gmtoff)
			s = fmt.Sprintf("%c%0*d", dchSignChar(tm.gmtoff), w, a/pgSecsPerHour)
			if a%pgSecsPerHour != 0 {
				s += fmt.Sprintf(":%02d", a%pgSecsPerHour/pgSecsPerMinute)
			}
		case dchA_D, dchB_C:
			s = map[bool]string{true: "B.C.", false: "A.D."}[tm.year <= 0]
		case dchAD, dchBC:
			s = map[bool]string{true: "BC", false: "AD"}[tm.year <= 0]
		case dcha_d, dchb_c:
			s = map[bool]string{true: "b.c.", false: "a.d."}[tm.year <= 0]
		case dchad, dchbc:
			s = map[bool]string{true: "bc", false: "ad"}[tm.year <= 0]
		case dchMONTH, dchMonth, dchmonth:
			if tm.mon == 0 {
				break
			}
			name(dchMonthsFull[tm.mon-1], id == dchMONTH, id == dchmonth, true)
		case dchMON, dchMon, dchmon:
			if tm.mon == 0 {
				break
			}
			name(pgMonthNames[tm.mon-1], id == dchMON, id == dchmon, false)
		case dchMM:
			pad(width(2, 3, tm.mon), tm.mon)
		case dchDAY, dchDay, dchday:
			name(pgDayNames[tm.wday], id == dchDAY, id == dchday, true)
		case dchDY, dchDy, dchdy:
			name(dchDaysShort[tm.wday], id == dchDY, id == dchdy, false)
		case dchDDD:
			pad(width(3, 3, 0), tm.yday)
		case dchIDDD:
			pad(width(3, 3, 0), dchDate2isoyearday(tm.year, tm.mon, tm.mday))
		case dchDD:
			pad(width(2, 2, 0), tm.mday)
		case dchD:
			pad(0, tm.wday+1)
		case dchID:
			d := tm.wday
			if d == 0 {
				d = 7
			}
			pad(0, d)
		case dchWW:
			pad(width(2, 2, 0), (tm.yday-1)/7+1)
		case dchIW:
			pad(width(2, 2, 0), date2isoweek(tm.year, tm.mon, tm.mday))
		case dchQ:
			if tm.mon == 0 {
				break
			}
			pad(0, (tm.mon-1)/3+1)
		case dchCC:
			var i int32
			switch {
			case isInterval:
				i = tm.year / 100
			case tm.year > 0:
				i = (tm.year-1)/100 + 1
			default:
				i = tm.year/100 - 1
			}
			if i <= 99 && i >= -99 {
				pad(width(2, 3, i), i)
			} else {
				pad(0, i)
			}
		case dchY_YYY:
			y := dchAdjustYear(tm.year, isInterval)
			i := y / 1000
			s, num = fmt.Sprintf("%d,%03d", i, y-i*1000), true
		case dchYYYY, dchIYYY:
			pad(width(4, 5, dchAdjustYear(tm.year, isInterval)), yearOf(id == dchIYYY))
		case dchYYY, dchIYY:
			pad(width(3, 4, dchAdjustYear(tm.year, isInterval)), yearOf(id == dchIYY)%1000)
		case dchYY, dchIY:
			pad(width(2, 3, dchAdjustYear(tm.year, isInterval)), yearOf(id == dchIY)%100)
		case dchY, dchI:
			pad(1, yearOf(id == dchI)%10)
		case dchRM, dchrm:
			if tm.mon == 0 && tm.year == 0 {
				break
			}
			months := dchRMUpper
			if id == dchrm {
				months = dchRMLower
			}
			var mon int32
			switch {
			case tm.mon == 0:
				if tm.year < 0 {
					mon = pgMonthsPerYear - 1
				}
			case tm.mon < 0:
				mon = -1 * (tm.mon + 1)
			default:
				mon = pgMonthsPerYear - tm.mon
			}
			if n.fm() {
				s = months[mon]
			} else {
				s = fmt.Sprintf("%-4s", months[mon])
			}
		case dchW:
			pad(0, (tm.mday-1)/7+1)
		case dchJ:
			pad(0, date2j(tm.year, tm.mon, tm.mday))
		}
		if num && n.thth() {
			th, err := pgNumGetTh([]byte(s), n.suffix&dchSuffTH != 0)
			if err != nil {
				return "", err
			}
			s += th
		}
		b.WriteString(s)
	}
	return b.String(), nil
}

func dchSignChar(gmtoff int64) byte {
	if gmtoff >= 0 {
		return '+'
	}
	return '-'
}

// dchAbsInt is abs((int) x).
func dchAbsInt(x int64) int32 {
	v := int32(x)
	if v < 0 {
		v = -v
	}
	return v
}

// dchToCharBody is datetime_to_char_body's caller's part: NULL for an empty picture.
func dchToCharBody(f string, isInterval bool, tm pgTm, fsec int64, tzn *string) (any, error) {
	if len(f) == 0 {
		return nil, nil
	}
	s, err := dchToChar(dchParse(f), isInterval, tm, fsec, tzn)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// dchTimestampToChar is timestamp_to_char and timestamptz_to_char.
func dchTimestampToChar(dt int64, withZone bool, f string) (any, error) {
	if len(f) == 0 || pgTimestampNotFinite(dt) {
		return nil, nil
	}
	tm, fsec, _, tzn, ok := timestamp2tm(dt, withZone, pgSessionZone)
	if !ok {
		return nil, dchErrTimestamp
	}
	if !withZone {
		tm.gmtoff = 0
	}
	thisdate := date2j(tm.year, tm.mon, tm.mday)
	tm.wday = (thisdate + 1) % 7
	tm.yday = thisdate - date2j(tm.year, 1, 1) + 1
	var zone *string
	if withZone {
		zone = &tzn
	}
	return dchToCharBody(f, false, tm, fsec, zone)
}

// dchIntervalToChar is interval_to_char.
func dchIntervalToChar(iv pgInterval, f string) (any, error) {
	if len(f) == 0 {
		return nil, nil
	}
	tm, fsec, err := interval2tm(iv)
	if err != nil {
		return nil, nil
	}
	tm.wday = 0
	tm.yday = (tm.year*pgMonthsPerYear+tm.mon)*pgDaysPerMonth + tm.mday
	return dchToCharBody(f, true, tm, fsec, nil)
}

// dchTmFromChar is TmFromChar.
type dchTmFromChar struct {
	mode                                                                       int
	hh, pm, mi, ss, ssss, d, dd, ddd, mm, ms, year, bc, ww, w, cc, j, us, yysz int32
	clock, tzsign, tzh, tzm, ff                                                int32
}

// dchFromCharState is DCH_from_char's walk over the input.
type dchFromCharState struct {
	s     string
	pos   int
	nodes []dchNode
}

func (st *dchFromCharState) at(i int) byte {
	if i >= 0 && i < len(st.s) {
		return st.s[i]
	}
	return 0
}

// mblen is pg_mblen at i, 1 at the end as at a NUL.
func (st *dchFromCharState) mblen(i int) int {
	if i >= len(st.s) {
		return 1
	}
	return pgNumMblen(st.s[i:])
}

// isNextSeparator is is_next_separator for node i.
func (st *dchFromCharState) isNextSeparator(i int) bool {
	n := st.nodes[i]
	if n.typ == dchNodeAction && n.thth() {
		return true
	}
	if i+1 >= len(st.nodes) {
		return true
	}
	m := st.nodes[i+1]
	if m.typ == dchNodeAction {
		return !m.key.isDigit
	}
	if len(m.chr) == 1 && cIsDigit(m.chr[0]) {
		return false
	}
	return true
}

// dchStrtol is strtol in base 10 over a 64-bit long: the value, where it stopped (i where there
// are no digits) and ERANGE.
func dchStrtol(s string, i int) (int64, int, bool) {
	start := i
	for i < len(s) && cIsSpace(s[i]) {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	d := i
	var v uint64
	over := false
	for i < len(s) && cIsDigit(s[i]) {
		c := uint64(s[i] - '0')
		if v > (math.MaxUint64-c)/10 {
			over = true
		} else {
			v = v*10 + c
		}
		i++
	}
	if i == d {
		return 0, start, false
	}
	switch {
	case neg && (over || v > 1<<63):
		return math.MinInt64, i, true
	case !neg && (over || v > math.MaxInt64):
		return math.MaxInt64, i, true
	case neg:
		return -int64(v), i, false
	}
	return int64(v), i, false
}

// setInt is from_char_set_int.
func dchSetInt(dest *int32, value int32, n dchNode) error {
	if *dest != 0 && *dest != value {
		return pgErrorf("conflicting values for \"%s\" field in formatting string", n.key.name)
	}
	*dest = value
	return nil
}

// parseIntLen is from_char_parse_int_len for node i; dest nil discards the value.
func (st *dchFromCharState) parseIntLen(dest *int32, ln int, i int) (int, error) {
	n := st.nodes[i]
	init := st.pos
	for st.pos < len(st.s) && cIsSpace(st.s[st.pos]) {
		st.pos++
	}
	rest := st.s[st.pos:]
	used := len(rest)
	cp := rest[:min(ln, len(rest))]
	var result int64
	var erange bool
	if n.fm() || st.isNextSeparator(i) {
		result, st.pos, erange = dchStrtol(st.s, init)
	} else {
		if used < ln {
			return -1, pgErrorf("source string too short for \"%s\" formatting field", n.key.name)
		}
		var last int
		result, last, erange = dchStrtol(cp, 0)
		if last > 0 && last < ln {
			return -1, pgErrorf("invalid value \"%s\" for \"%s\"", cp, n.key.name)
		}
		st.pos += last
	}
	if st.pos == init {
		return -1, pgErrorf("invalid value \"%s\" for \"%s\"", cp, n.key.name)
	}
	if erange || result < math.MinInt32 || result > math.MaxInt32 {
		return -1, pgErrorf("value for \"%s\" in source string is out of range", n.key.name)
	}
	if dest != nil {
		if err := dchSetInt(dest, int32(result), n); err != nil {
			return -1, err
		}
	}
	return st.pos - init, nil
}

func (st *dchFromCharState) parseInt(dest *int32, i int) (int, error) {
	return st.parseIntLen(dest, len(st.nodes[i].key.name), i)
}

// seqSearch is from_char_seq_search with seq_search_ascii; the localized arrays of lc_time C are
// the English ones, matched alike.
func (st *dchFromCharState) seqSearch(array []string, i int) (int32, error) {
	name := st.s[st.pos:]
	if name != "" {
		first := asciiLower(name[0])
		for k, a := range array {
			if asciiLower(a[0]) != first {
				continue
			}
			if len(name) >= len(a) && asciiMap(name[1:len(a)], asciiLower) == asciiMap(a[1:], asciiLower) {
				st.pos += len(a)
				return int32(k), nil
			}
		}
	}
	cp := name
	for j := 0; j < len(cp); j++ {
		if c := cp[j]; c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' {
			cp = cp[:j]
			break
		}
	}
	return 0, pgErrorf("invalid value \"%s\" for \"%s\"", cp, st.nodes[i].key.name)
}

// skipTHth is SKIP_THth.
func (st *dchFromCharState) skipTHth(n dchNode) {
	if n.thth() {
		for k := 0; k < 2; k++ {
			if st.pos < len(st.s) {
				st.pos += st.mblen(st.pos)
			}
		}
	}
}

// dchScanInt is glibc sscanf's %d with a maximum width (0 for none): the value as an int of the
// long strtol gives, the end, and whether it matched.
func dchScanInt(s string, i, width int) (int32, int, bool) {
	for i < len(s) && cIsSpace(s[i]) {
		i++
	}
	end := len(s)
	if width > 0 {
		end = min(end, i+width)
	}
	j := i
	if j < end && (s[j] == '+' || s[j] == '-') {
		j++
	}
	d := j
	for j < end && cIsDigit(s[j]) {
		j++
	}
	if j == d {
		return 0, i, false
	}
	v, _, _ := dchStrtol(s[i:j], 0)
	return int32(v), j, true
}

// dchFromChar is DCH_from_char, outside standard mode.
func dchFromChar(nodes []dchNode, in string, out *dchTmFromChar) error {
	st := &dchFromCharState{s: in, nodes: nodes}
	fx := false
	extraSkip := 0
	isSpace := func(c byte) bool { return cIsSpace(c) }
	i := 0
	for ; i < len(nodes) && st.pos < len(in); i++ {
		n := nodes[i]
		if !fx && (n.typ != dchNodeAction || n.key.id != dchFX) && (n.typ == dchNodeAction || i == 0) {
			for st.pos < len(in) && isSpace(in[st.pos]) {
				st.pos++
				extraSkip++
			}
		}
		if n.typ == dchNodeSpace || n.typ == dchNodeSeparator {
			if !fx {
				extraSkip--
				if c := st.at(st.pos); isSpace(c) || dchIsSeparatorChar(c) {
					st.pos++
					extraSkip++
				}
			} else {
				st.pos += st.mblen(st.pos)
			}
			continue
		} else if n.typ != dchNodeAction {
			if !fx && extraSkip > 0 {
				extraSkip--
			} else {
				st.pos += st.mblen(st.pos)
			}
			continue
		}
		if n.key.mode != dchModeNone {
			if out.mode == dchModeNone {
				out.mode = n.key.mode
			} else if out.mode != n.key.mode {
				return pgErrorf("invalid combination of date conventions")
			}
		}
		var err error
		var ln int
		var value int32
		switch n.key.id {
		case dchFX:
			fx = true
		case dchA_M, dchP_M, dcha_m, dchp_m, dchAM, dchPM, dcham, dchpm:
			arr := dchAmPm
			if n.key.id == dchA_M || n.key.id == dchP_M || n.key.id == dcha_m || n.key.id == dchp_m {
				arr = dchAmPmLong
			}
			if value, err = st.seqSearch(arr, i); err == nil {
				err = dchSetInt(&out.pm, value%2, n)
			}
			out.clock = 1
		case dchHH, dchHH12:
			_, err = st.parseIntLen(&out.hh, 2, i)
			out.clock = 1
			st.skipTHth(n)
		case dchHH24:
			_, err = st.parseIntLen(&out.hh, 2, i)
			st.skipTHth(n)
		case dchMI:
			_, err = st.parseInt(&out.mi, i)
			st.skipTHth(n)
		case dchSS:
			_, err = st.parseInt(&out.ss, i)
			st.skipTHth(n)
		case dchMS:
			if ln, err = st.parseIntLen(&out.ms, 3, i); err == nil {
				switch ln {
				case 1:
					out.ms *= 100
				case 2:
					out.ms *= 10
				}
			}
			st.skipTHth(n)
		case dchFF1, dchFF2, dchFF3, dchFF4, dchFF5, dchFF6, dchUS:
			w := 6
			if n.key.id != dchUS {
				out.ff = int32(n.key.id - dchFF1 + 1)
				w = int(out.ff)
			}
			if ln, err = st.parseIntLen(&out.us, w, i); err == nil {
				out.us *= [...]int32{1, 100000, 10000, 1000, 100, 10, 1}[min(max(ln, 0), 6)]
			}
			st.skipTHth(n)
		case dchSSSS:
			_, err = st.parseInt(&out.ssss, i)
			st.skipTHth(n)
		case dchtz, dchTZ, dchOF:
			err = pgErrorf("formatting field \"%s\" is only supported in to_char", n.key.name)
		case dchTZH:
			if c := st.at(st.pos); c == '+' || c == '-' || c == ' ' {
				out.tzsign = 1
				if c == '-' {
					out.tzsign = -1
				}
				st.pos++
			} else if extraSkip > 0 && st.at(st.pos-1) == '-' {
				out.tzsign = -1
			} else {
				out.tzsign = 1
			}
			_, err = st.parseIntLen(&out.tzh, 2, i)
		case dchTZM:
			if out.tzsign == 0 {
				out.tzsign = 1
			}
			_, err = st.parseIntLen(&out.tzm, 2, i)
		case dchA_D, dchB_C, dcha_d, dchb_c, dchAD, dchBC, dchad, dchbc:
			arr := dchAdBc
			if n.key.id == dchA_D || n.key.id == dchB_C || n.key.id == dcha_d || n.key.id == dchb_c {
				arr = dchAdBcLong
			}
			if value, err = st.seqSearch(arr, i); err == nil {
				err = dchSetInt(&out.bc, value%2, n)
			}
		case dchMONTH, dchMonth, dchmonth:
			if value, err = st.seqSearch(dchMonthsFull, i); err == nil {
				err = dchSetInt(&out.mm, value+1, n)
			}
		case dchMON, dchMon, dchmon:
			if value, err = st.seqSearch(pgMonthNames, i); err == nil {
				err = dchSetInt(&out.mm, value+1, n)
			}
		case dchMM:
			_, err = st.parseInt(&out.mm, i)
			st.skipTHth(n)
		case dchDAY, dchDay, dchday, dchDY, dchDy, dchdy:
			arr := pgDayNames
			if n.key.id == dchDY || n.key.id == dchDy || n.key.id == dchdy {
				arr = dchDaysShort
			}
			if value, err = st.seqSearch(arr, i); err == nil {
				err = dchSetInt(&out.d, value, n)
				out.d++
			}
		case dchDDD:
			_, err = st.parseInt(&out.ddd, i)
			st.skipTHth(n)
		case dchIDDD:
			_, err = st.parseIntLen(&out.ddd, 3, i)
			st.skipTHth(n)
		case dchDD:
			_, err = st.parseInt(&out.dd, i)
			st.skipTHth(n)
		case dchD:
			_, err = st.parseInt(&out.d, i)
			st.skipTHth(n)
		case dchID:
			if _, err = st.parseIntLen(&out.d, 1, i); err == nil {
				if out.d++; out.d > 7 {
					out.d = 1
				}
			}
			st.skipTHth(n)
		case dchWW, dchIW:
			_, err = st.parseInt(&out.ww, i)
			st.skipTHth(n)
		case dchQ:
			_, err = st.parseInt(nil, i)
			st.skipTHth(n)
		case dchCC:
			_, err = st.parseInt(&out.cc, i)
			st.skipTHth(n)
		case dchY_YYY:
			millennia, e1, ok1 := dchScanInt(in, st.pos, 0)
			ok := ok1 && e1 < len(in) && in[e1] == ','
			var years int32
			var e2 int
			if ok {
				years, e2, ok = dchScanInt(in, e1+1, 3)
			}
			if !ok {
				return pgErrorf("invalid input string for \"Y,YYY\"")
			}
			years += millennia * 1000
			if err = dchSetInt(&out.year, years, n); err == nil {
				out.yysz = 4
				st.pos = e2
				st.skipTHth(n)
			}
		case dchYYYY, dchIYYY:
			_, err = st.parseInt(&out.year, i)
			out.yysz = 4
			st.skipTHth(n)
		case dchYYY, dchIYY, dchYY, dchIY, dchY, dchI:
			if ln, err = st.parseInt(&out.year, i); err == nil && ln < 4 {
				out.year = dchAdjustPartialYear(out.year)
			}
			out.yysz = int32(len(n.key.name))
			st.skipTHth(n)
		case dchRM, dchrm:
			if value, err = st.seqSearch(dchRMLower, i); err == nil {
				err = dchSetInt(&out.mm, pgMonthsPerYear-value, n)
			}
		case dchW:
			_, err = st.parseInt(&out.w, i)
			st.skipTHth(n)
		case dchJ:
			_, err = st.parseInt(&out.j, i)
			st.skipTHth(n)
		}
		if err != nil {
			return err
		}
		if !fx {
			extraSkip = 0
			for st.pos < len(in) && isSpace(in[st.pos]) {
				st.pos++
				extraSkip++
			}
		}
	}
	return nil
}

// dchAdjustPartialYear is adjust_partial_year_to_2020.
func dchAdjustPartialYear(year int32) int32 {
	switch {
	case year < 70:
		return year + 2000
	case year < 100:
		return year + 1900
	case year < 520:
		return year + 2000
	case year < 1000:
		return year + 1000
	}
	return year
}

// dchDoToTimestamp is do_to_timestamp outside standard mode: the broken-down time, its fraction,
// the fractional precision FF gave, and the zone the text gave, "" for none.
func dchDoToTimestamp(dateStr, f string) (pgTm, int64, int32, string, error) {
	var tmfc dchTmFromChar
	tm := pgTm{mon: 1, mday: 1}
	var fsec int64
	fmask := 0
	if len(f) > 0 {
		if err := dchFromChar(dchParse(f), dateStr, &tmfc); err != nil {
			return tm, 0, 0, "", err
		}
	}
	if tmfc.ssss != 0 {
		x := tmfc.ssss
		tm.hour = x / pgSecsPerHour
		x %= pgSecsPerHour
		tm.min = x / pgSecsPerMinute
		x %= pgSecsPerMinute
		tm.sec = x
	}
	if tmfc.ss != 0 {
		tm.sec = tmfc.ss
	}
	if tmfc.mi != 0 {
		tm.min = tmfc.mi
	}
	if tmfc.hh != 0 {
		tm.hour = tmfc.hh
	}
	if tmfc.clock == 1 {
		if tm.hour < 1 || tm.hour > pgHoursPerDay/2 {
			return tm, 0, 0, "", pgErrorf("hour \"%d\" is invalid for the 12-hour clock", tm.hour)
		}
		if tmfc.pm != 0 && tm.hour < pgHoursPerDay/2 {
			tm.hour += pgHoursPerDay / 2
		} else if tmfc.pm == 0 && tm.hour == pgHoursPerDay/2 {
			tm.hour = 0
		}
	}
	if tmfc.year != 0 {
		if tmfc.cc != 0 && tmfc.yysz <= 2 {
			if tmfc.bc != 0 {
				tmfc.cc = -tmfc.cc
			}
			tm.year = tmfc.year % 100
			if tm.year != 0 {
				if tmfc.cc >= 0 {
					tm.year += (tmfc.cc - 1) * 100
				} else {
					tm.year = (tmfc.cc+1)*100 - tm.year + 1
				}
			} else {
				tm.year = tmfc.cc * 100
				if tmfc.cc < 0 {
					tm.year++
				}
			}
		} else {
			tm.year = tmfc.year
			if tmfc.bc != 0 {
				tm.year = -tm.year
			}
			if tm.year < 0 {
				tm.year++
			}
		}
		fmask |= dtkM(tokYear)
	} else if tmfc.cc != 0 {
		if tmfc.bc != 0 {
			tmfc.cc = -tmfc.cc
		}
		if tmfc.cc >= 0 {
			tm.year = (tmfc.cc-1)*100 + 1
		} else {
			tm.year = tmfc.cc*100 + 1
		}
		fmask |= dtkM(tokYear)
	}
	if tmfc.j != 0 {
		tm.year, tm.mon, tm.mday = j2date(tmfc.j)
		fmask |= dtkDateM
	}
	if tmfc.ww != 0 {
		if tmfc.mode == dchModeISOWeek {
			jday := isoweek2j(tm.year, tmfc.ww)
			if tmfc.d != 0 {
				if tmfc.d > 1 {
					jday += tmfc.d - 2
				} else {
					jday += 6
				}
			}
			tm.year, tm.mon, tm.mday = j2date(jday)
			fmask |= dtkDateM
		} else {
			tmfc.ddd = (tmfc.ww-1)*7 + 1
		}
	}
	if tmfc.w != 0 {
		tmfc.dd = (tmfc.w-1)*7 + 1
	}
	if tmfc.dd != 0 {
		tm.mday = tmfc.dd
		fmask |= dtkM(tokDay)
	}
	if tmfc.mm != 0 {
		tm.mon = tmfc.mm
		fmask |= dtkM(tokMonth)
	}
	if tmfc.ddd != 0 && (tm.mon <= 1 || tm.mday <= 1) {
		if tm.year == 0 && tmfc.bc == 0 {
			return tm, 0, 0, "", pgErrorf("cannot calculate day of year without year information")
		}
		if tmfc.mode == dchModeISOWeek {
			j0 := isoweek2j(tm.year, 1) - 1
			tm.year, tm.mon, tm.mday = j2date(j0 + tmfc.ddd)
			fmask |= dtkDateM
		} else {
			ysum := [2][13]int32{
				{0, 31, 59, 90, 120, 151, 181, 212, 243, 273, 304, 334, 365},
				{0, 31, 60, 91, 121, 152, 182, 213, 244, 274, 305, 335, 366}}
			y := ysum[pgIsLeap(tm.year)]
			i := int32(1)
			for ; i <= pgMonthsPerYear; i++ {
				if tmfc.ddd <= y[i] {
					break
				}
			}
			if tm.mon <= 1 {
				tm.mon = i
			}
			if tm.mday <= 1 {
				tm.mday = tmfc.ddd - y[i-1]
			}
			fmask |= dtkM(tokMonth) | dtkM(tokDay)
		}
	}
	if tmfc.ms != 0 {
		fsec += int64(tmfc.ms * 1000)
	}
	if tmfc.us != 0 {
		fsec += int64(tmfc.us)
	}
	if fmask != 0 {
		if validateDate(fmask, true, false, false, &tm) != 0 {
			return tm, 0, 0, "", dateTimeParseError(dtErrFieldOverflow, dateStr, "timestamp")
		}
	}
	if tm.hour < 0 || tm.hour >= pgHoursPerDay || tm.min < 0 || tm.min >= pgMinsPerHour ||
		tm.sec < 0 || tm.sec >= pgSecsPerMinute || fsec < 0 || fsec >= pgUsecsPerSec {
		return tm, 0, 0, "", dateTimeParseError(dtErrFieldOverflow, dateStr, "timestamp")
	}
	zone := ""
	if tmfc.tzsign != 0 {
		if tmfc.tzh < 0 || tmfc.tzh > pgMaxTZDispHour || tmfc.tzm < 0 || tmfc.tzm >= pgMinsPerHour {
			return tm, 0, 0, "", dateTimeParseError(dtErrTZDispOverflow, dateStr, "timestamp")
		}
		sign := '+'
		if tmfc.tzsign < 0 {
			sign = '-'
		}
		zone = fmt.Sprintf("%c%02d:%02d", sign, tmfc.tzh, tmfc.tzm)
	}
	return tm, fsec, tmfc.ff, zone, nil
}

// dchToTimestamp is to_timestamp(text, text).
func dchToTimestamp(dateStr, f string) (any, error) {
	tm, fsec, fprec, zone, err := dchDoToTimestamp(dateStr, f)
	if err != nil {
		return nil, err
	}
	var tz int32
	if zone != "" {
		if dterr := decodeTimezone(zone, &tz); dterr != 0 {
			return nil, dateTimeParseError(dterr, dateStr, "timestamptz")
		}
	} else {
		tz, _ = determineTimeZoneOffset(&tm, pgSessionZone)
	}
	result, ok := tm2timestamp(tm, fsec, &tz)
	if !ok {
		return nil, dchErrTimestamp
	}
	if fprec != 0 {
		result = dchAdjustTimestampForTypmod(result, int(fprec))
	}
	return pgTimestamptz(result), nil
}

// dchAdjustTimestampForTypmod is AdjustTimestampForTypmod for a precision of 1 to 6.
func dchAdjustTimestampForTypmod(t int64, typmod int) int64 {
	if pgTimestampNotFinite(t) || typmod == pgMaxTimestampPrecision {
		return t
	}
	scales := [...]int64{1000000, 100000, 10000, 1000, 100, 10, 1}
	offsets := [...]int64{500000, 50000, 5000, 500, 50, 5, 0}
	if t >= 0 {
		return (t + offsets[typmod]) / scales[typmod] * scales[typmod]
	}
	return -(((-t + offsets[typmod]) / scales[typmod]) * scales[typmod])
}

func init() {
	registerPg("to_char(timestamp,text)", func(a []any) (any, error) {
		return dchTimestampToChar(int64(a[0].(pgTimestamp)), false, a[1].(string))
	})
	registerPg("to_char(timestamptz,text)", func(a []any) (any, error) {
		return dchTimestampToChar(int64(a[0].(pgTimestamptz)), true, a[1].(string))
	})
	registerPg("to_char(interval,text)", func(a []any) (any, error) {
		return dchIntervalToChar(a[0].(pgInterval), a[1].(string))
	})
	registerPg("to_timestamp(text,text)", func(a []any) (any, error) {
		return dchToTimestamp(a[0].(string), a[1].(string))
	})
}
