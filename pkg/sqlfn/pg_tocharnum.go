package sqlfn

import (
	"math"
	"strconv"
	"strings"
)

// to_char over numbers, ported from Postgres 14's formatting.c (the NUM_* half): the format picture
// parsed to nodes and a NUMDesc, the number printed as its type's to_char prints it, and the two
// merged by NUM_processor. The server runs lc_numeric and lc_monetary "C", whose empty lconv
// strings NUM_prepare_locale replaces with the defaults: '-', '+', '.', ',' and a blank currency.

const (
	pgNumComma = iota
	pgNumDec
	pgNum0
	pgNum9
	pgNumB
	pgNumC
	pgNumD
	pgNumE
	pgNumFM
	pgNumG
	pgNumL
	pgNumMI
	pgNumPL
	pgNumPR
	pgNumRN
	pgNumSG
	pgNumSP
	pgNumS
	pgNumTH
	pgNumV
	pgNumRNLower
	pgNumTHLower
)

// pgNumKeywords is NUM_keywords, in its search order.
var pgNumKeywords = []struct {
	name string
	id   int
}{
	{",", pgNumComma}, {".", pgNumDec}, {"0", pgNum0}, {"9", pgNum9}, {"B", pgNumB}, {"C", pgNumC},
	{"D", pgNumD}, {"EEEE", pgNumE}, {"FM", pgNumFM}, {"G", pgNumG}, {"L", pgNumL}, {"MI", pgNumMI},
	{"PL", pgNumPL}, {"PR", pgNumPR}, {"RN", pgNumRN}, {"SG", pgNumSG}, {"SP", pgNumSP}, {"S", pgNumS},
	{"TH", pgNumTH}, {"V", pgNumV}, {"b", pgNumB}, {"c", pgNumC}, {"d", pgNumD}, {"eeee", pgNumE},
	{"fm", pgNumFM}, {"g", pgNumG}, {"l", pgNumL}, {"mi", pgNumMI}, {"pl", pgNumPL}, {"pr", pgNumPR},
	{"rn", pgNumRNLower}, {"sg", pgNumSG}, {"sp", pgNumSP}, {"s", pgNumS}, {"th", pgNumTHLower},
	{"v", pgNumV},
}

const (
	pgNumFDecimal   = 1 << 1
	pgNumFLDecimal  = 1 << 2
	pgNumFZero      = 1 << 3
	pgNumFBlank     = 1 << 4
	pgNumFFillMode  = 1 << 5
	pgNumFLSign     = 1 << 6
	pgNumFBracket   = 1 << 7
	pgNumFMinus     = 1 << 8
	pgNumFPlus      = 1 << 9
	pgNumFRoman     = 1 << 10
	pgNumFMulti     = 1 << 11
	pgNumFPlusPost  = 1 << 12
	pgNumFMinusPost = 1 << 13
	pgNumFEEEE      = 1 << 14

	pgNumLSignPre  = -1
	pgNumLSignPost = 1

	pgNumDblDig = 15
)

// pgNumDesc is NUMDesc.
type pgNumDesc struct {
	pre, post, lsign, flag, preLsignNum, multi, zeroStart, zeroEnd int
	needLocale                                                     bool
}

func (d *pgNumDesc) is(f int) bool { return d.flag&f != 0 }

// pgNumNode is a FormatNode: a keyword (key >= 0) or a character copied to the output.
type pgNumNode struct {
	key int
	chr string
}

// pgNumKeyword is index_seq_search over NUM_keywords.
func pgNumKeyword(s string) (string, int, bool) {
	for _, k := range pgNumKeywords {
		if strings.HasPrefix(s, k.name) {
			return k.name, k.id, true
		}
	}
	return "", 0, false
}

// pgNumMblen is pg_mblen in UTF8.
func pgNumMblen(s string) int {
	c := s[0]
	n := 1
	switch {
	case c&0xe0 == 0xc0:
		n = 2
	case c&0xf0 == 0xe0:
		n = 3
	case c&0xf8 == 0xf0:
		n = 4
	}
	return min(n, len(s))
}

func pgNumSyntax(msg string) error { return pgErrorf("%s", msg) }

// prepare is NUMDesc_prepare.
func (d *pgNumDesc) prepare(id int) error {
	if d.is(pgNumFEEEE) && id != pgNumE {
		return pgNumSyntax(`"EEEE" must be the last pattern used`)
	}
	switch id {
	case pgNum9:
		if d.is(pgNumFBracket) {
			return pgNumSyntax(`"9" must be ahead of "PR"`)
		}
		if d.is(pgNumFMulti) {
			d.multi++
			break
		}
		if d.is(pgNumFDecimal) {
			d.post++
		} else {
			d.pre++
		}
	case pgNum0:
		if d.is(pgNumFBracket) {
			return pgNumSyntax(`"0" must be ahead of "PR"`)
		}
		if !d.is(pgNumFZero) && !d.is(pgNumFDecimal) {
			d.flag |= pgNumFZero
			d.zeroStart = d.pre + 1
		}
		if !d.is(pgNumFDecimal) {
			d.pre++
		} else {
			d.post++
		}
		d.zeroEnd = d.pre + d.post
	case pgNumB:
		if d.pre == 0 && d.post == 0 && !d.is(pgNumFZero) {
			d.flag |= pgNumFBlank
		}
	case pgNumD, pgNumDec:
		if id == pgNumD {
			d.flag |= pgNumFLDecimal
			d.needLocale = true
		}
		if d.is(pgNumFDecimal) {
			return pgNumSyntax("multiple decimal points")
		}
		if d.is(pgNumFMulti) {
			return pgNumSyntax(`cannot use "V" and decimal point together`)
		}
		d.flag |= pgNumFDecimal
	case pgNumFM:
		d.flag |= pgNumFFillMode
	case pgNumS:
		if d.is(pgNumFLSign) {
			return pgNumSyntax(`cannot use "S" twice`)
		}
		if d.is(pgNumFPlus) || d.is(pgNumFMinus) || d.is(pgNumFBracket) {
			return pgNumSyntax(`cannot use "S" and "PL"/"MI"/"SG"/"PR" together`)
		}
		if !d.is(pgNumFDecimal) {
			d.lsign = pgNumLSignPre
			d.preLsignNum = d.pre
			d.needLocale = true
			d.flag |= pgNumFLSign
		} else if d.lsign == 0 {
			d.lsign = pgNumLSignPost
			d.needLocale = true
			d.flag |= pgNumFLSign
		}
	case pgNumMI:
		if d.is(pgNumFLSign) {
			return pgNumSyntax(`cannot use "S" and "MI" together`)
		}
		d.flag |= pgNumFMinus
		if d.is(pgNumFDecimal) {
			d.flag |= pgNumFMinusPost
		}
	case pgNumPL:
		if d.is(pgNumFLSign) {
			return pgNumSyntax(`cannot use "S" and "PL" together`)
		}
		d.flag |= pgNumFPlus
		if d.is(pgNumFDecimal) {
			d.flag |= pgNumFPlusPost
		}
	case pgNumSG:
		if d.is(pgNumFLSign) {
			return pgNumSyntax(`cannot use "S" and "SG" together`)
		}
		d.flag |= pgNumFMinus | pgNumFPlus
	case pgNumPR:
		if d.is(pgNumFLSign) || d.is(pgNumFPlus) || d.is(pgNumFMinus) {
			return pgNumSyntax(`cannot use "PR" and "S"/"PL"/"MI"/"SG" together`)
		}
		d.flag |= pgNumFBracket
	case pgNumRN, pgNumRNLower:
		d.flag |= pgNumFRoman
	case pgNumL, pgNumG:
		d.needLocale = true
	case pgNumV:
		if d.is(pgNumFDecimal) {
			return pgNumSyntax(`cannot use "V" and decimal point together`)
		}
		d.flag |= pgNumFMulti
	case pgNumE:
		if d.is(pgNumFEEEE) {
			return pgNumSyntax(`cannot use "EEEE" twice`)
		}
		if d.is(pgNumFBlank | pgNumFFillMode | pgNumFLSign | pgNumFBracket | pgNumFMinus | pgNumFPlus |
			pgNumFRoman | pgNumFMulti) {
			return pgNumSyntax(`"EEEE" is incompatible with other formats`)
		}
		d.flag |= pgNumFEEEE
	}
	return nil
}

// pgNumParse is NUM_cache's parse_format with NUM_FLAG: the picture's nodes and its NUMDesc.
func pgNumParse(str string) ([]pgNumNode, pgNumDesc, error) {
	var d pgNumDesc
	var nodes []pgNumNode
	for len(str) > 0 {
		if name, id, ok := pgNumKeyword(str); ok {
			str = str[len(name):]
			if err := d.prepare(id); err != nil {
				return nil, d, err
			}
			nodes = append(nodes, pgNumNode{key: id})
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
				n := pgNumMblen(str)
				nodes = append(nodes, pgNumNode{key: -1, chr: str[:n]})
				str = str[n:]
			}
			continue
		}
		if str[0] == '\\' && len(str) > 1 && str[1] == '"' {
			str = str[1:]
		}
		n := pgNumMblen(str)
		nodes = append(nodes, pgNumNode{key: -1, chr: str[:n]})
		str = str[n:]
	}
	return nodes, d, nil
}

// pgNumIntToRoman is int_to_roman.
func pgNumIntToRoman(number int64) string {
	if number > 3999 || number < 1 {
		return strings.Repeat("#", 15)
	}
	rm1 := []string{"I", "II", "III", "IV", "V", "VI", "VII", "VIII", "IX"}
	rm10 := []string{"X", "XX", "XXX", "XL", "L", "LX", "LXX", "LXXX", "XC"}
	rm100 := []string{"C", "CC", "CCC", "CD", "D", "DC", "DCC", "DCCC", "CM"}
	var b strings.Builder
	s := strconv.FormatInt(number, 10)
	for i := 0; i < len(s); i++ {
		l := len(s) - i
		num := int(s[i]) - '1'
		if num < 0 {
			continue
		}
		switch {
		case l > 3:
			b.WriteString(strings.Repeat("M", num+1))
		case l == 3:
			b.WriteString(rm100[num])
		case l == 2:
			b.WriteString(rm10[num])
		case l == 1:
			b.WriteString(rm1[num])
		}
	}
	return b.String()
}

// pgNumGetTh is get_th.
func pgNumGetTh(num []byte, upper bool) (string, error) {
	l := len(num)
	last := byte(0)
	if l > 0 {
		last = num[l-1]
	}
	if last < '0' || last > '9' {
		return "", pgErrorf("\"%s\" is not a number", num)
	}
	if l > 1 && num[l-2] == '1' {
		last = 0
	}
	th := [4]string{"th", "st", "nd", "rd"}
	i := 0
	if last >= '1' && last <= '3' {
		i = int(last - '0')
	}
	if upper {
		return strings.ToUpper(th[i]), nil
	}
	return th[i], nil
}

// pgNumProc is NUMProc, for to_char: indexes stand for its pointers, -1 for NULL.
type pgNumProc struct {
	num          *pgNumDesc
	sign         int
	signWrote    bool
	numCount     int
	numIn        bool
	numCurr      int
	outPreSpaces int
	number       []byte
	numberP      int
	lastRelevant int
	out          []byte
	p            int
}

// at is number[i], NUL past its end.
func (np *pgNumProc) at(i int) byte {
	if i >= 0 && i < len(np.number) {
		return np.number[i]
	}
	return 0
}

func (np *pgNumProc) put(i int, c byte) {
	for len(np.out) <= i {
		np.out = append(np.out, 0)
	}
	np.out[i] = c
}

// strcpy copies s and its terminating NUL to the output at p.
func (np *pgNumProc) strcpy(s string) {
	for i := 0; i < len(s); i++ {
		np.put(np.p+i, s[i])
	}
	np.put(np.p+len(s), 0)
}

func (np *pgNumProc) isPredecSpace() bool {
	return !np.num.is(pgNumFZero) && np.numberP == 0 && np.at(0) == '0' && np.num.post != 0
}

func (np *pgNumProc) lastRelevantIsDot() bool {
	return np.lastRelevant >= 0 && np.at(np.lastRelevant) == '.'
}

// numpart is NUM_numpart_to_char.
func (np *pgNumProc) numpart(id int) {
	d := np.num
	if d.is(pgNumFRoman) {
		return
	}
	np.numIn = false
	if !np.signWrote &&
		(np.numCurr >= np.outPreSpaces || (d.is(pgNumFZero) && d.zeroStart == np.numCurr)) &&
		(!np.isPredecSpace() || np.lastRelevantIsDot()) {
		switch {
		case d.is(pgNumFLSign):
			if d.lsign == pgNumLSignPre {
				if np.sign == '-' {
					np.strcpy("-")
				} else {
					np.strcpy("+")
				}
				np.p++
				np.signWrote = true
			}
		case d.is(pgNumFBracket):
			if np.sign == '+' {
				np.put(np.p, ' ')
			} else {
				np.put(np.p, '<')
			}
			np.p++
			np.signWrote = true
		case np.sign == '+':
			if !d.is(pgNumFFillMode) {
				np.put(np.p, ' ')
				np.p++
			}
			np.signWrote = true
		case np.sign == '-':
			np.put(np.p, '-')
			np.p++
			np.signWrote = true
		}
	}
	if id == pgNum9 || id == pgNum0 || id == pgNumD || id == pgNumDec {
		switch {
		case np.numCurr < np.outPreSpaces && (d.zeroStart > np.numCurr || !d.is(pgNumFZero)):
			if !d.is(pgNumFFillMode) {
				np.put(np.p, ' ')
				np.p++
			}
		case d.is(pgNumFZero) && np.numCurr < np.outPreSpaces && d.zeroStart <= np.numCurr:
			np.put(np.p, '0')
			np.p++
			np.numIn = true
		default:
			if np.at(np.numberP) == '.' {
				if !np.lastRelevantIsDot() || d.is(pgNumFFillMode) {
					np.strcpy(".")
					np.p++
				}
			} else {
				switch {
				case np.lastRelevant >= 0 && np.numberP > np.lastRelevant && id != pgNum0:
				case np.isPredecSpace():
					if !d.is(pgNumFFillMode) {
						np.put(np.p, ' ')
						np.p++
					} else if np.lastRelevantIsDot() {
						np.put(np.p, '0')
						np.p++
					}
				default:
					np.put(np.p, np.at(np.numberP))
					np.p++
					np.numIn = true
				}
			}
			if np.at(np.numberP) != 0 {
				np.numberP++
			}
		}
		end := np.numCount
		if np.outPreSpaces != 0 {
			end++
		}
		if d.is(pgNumFDecimal) {
			end++
		}
		if np.lastRelevant >= 0 && np.lastRelevant == np.numberP {
			end = np.numCurr
		}
		if np.numCurr+1 == end {
			if np.signWrote && d.is(pgNumFBracket) {
				if np.sign == '+' {
					np.put(np.p, ' ')
				} else {
					np.put(np.p, '>')
				}
				np.p++
			} else if d.is(pgNumFLSign) && d.lsign == pgNumLSignPost {
				if np.sign == '-' {
					np.strcpy("-")
				} else {
					np.strcpy("+")
				}
				np.p++
			}
		}
	}
	np.numCurr++
}

// pgNumProcess is NUM_processor for to_char, the result cut at its first NUL as the text it
// becomes.
func pgNumProcess(nodes []pgNumNode, d pgNumDesc, number string, outPreSpaces, sign int) (string, error) {
	np := &pgNumProc{num: &d, number: []byte(number), lastRelevant: -1}
	if d.zeroStart != 0 {
		d.zeroStart--
	}
	if d.is(pgNumFEEEE) {
		return pgNumCut([]byte(number)), nil
	}
	if d.is(pgNumFRoman) {
		d.lsign, d.preLsignNum, d.post, d.pre = 0, 0, 0, 0
		d.flag &= pgNumFFillMode
		d.flag |= pgNumFRoman
	}
	np.sign = sign
	if d.is(pgNumFPlus) || d.is(pgNumFMinus) {
		np.signWrote = !(d.is(pgNumFPlus) && !d.is(pgNumFMinus))
	} else {
		if np.sign != '-' {
			if d.is(pgNumFBracket) && d.is(pgNumFFillMode) {
				d.flag &^= pgNumFBracket
			}
			if d.is(pgNumFMinus) {
				d.flag &^= pgNumFMinus
			}
		} else if np.sign != '+' && d.is(pgNumFPlus) {
			d.flag &^= pgNumFPlus
		}
		np.signWrote = np.sign == '+' && d.is(pgNumFFillMode) && !d.is(pgNumFLSign)
		if d.lsign == pgNumLSignPre && d.pre == d.preLsignNum {
			d.lsign = pgNumLSignPost
		}
	}
	np.numCount = d.post + d.pre - 1
	np.outPreSpaces = outPreSpaces
	if d.is(pgNumFFillMode) && d.is(pgNumFDecimal) {
		np.lastRelevant = pgNumLastRelevantDecnum(np.number)
		if np.lastRelevant >= 0 && d.zeroEnd > np.outPreSpaces {
			if lastZero := d.zeroEnd - np.outPreSpaces; np.lastRelevant < lastZero {
				np.lastRelevant = lastZero
			}
		}
	}
	if !np.signWrote && np.outPreSpaces == 0 {
		np.numCount++
	}
	for _, n := range nodes {
		if n.key < 0 {
			np.strcpy(n.chr)
			np.p += len(n.chr)
			continue
		}
		switch n.key {
		case pgNum9, pgNum0, pgNumDec, pgNumD:
			np.numpart(n.key)
			continue
		case pgNumComma:
			if !np.numIn {
				if d.is(pgNumFFillMode) {
					continue
				}
				np.put(np.p, ' ')
			} else {
				np.put(np.p, ',')
			}
		case pgNumG:
			if !np.numIn {
				if d.is(pgNumFFillMode) {
					continue
				}
				np.put(np.p, ' ')
			} else {
				np.strcpy(",")
			}
		case pgNumL:
			np.strcpy(" ")
		case pgNumRN, pgNumRNLower:
			s := string(np.number[min(np.numberP, len(np.number)):])
			if i := strings.IndexByte(s, 0); i >= 0 {
				s = s[:i]
			}
			if n.key == pgNumRNLower {
				s = asciiMap(s, asciiLower)
			}
			if !d.is(pgNumFFillMode) && len(s) < 15 {
				s = strings.Repeat(" ", 15-len(s)) + s
			}
			np.strcpy(s)
			np.p += len(s) - 1
		case pgNumTH, pgNumTHLower:
			if d.is(pgNumFRoman) || np.at(0) == '#' || np.sign == '-' || d.is(pgNumFDecimal) {
				continue
			}
			th, err := pgNumGetTh(np.number, n.key == pgNumTH)
			if err != nil {
				return "", err
			}
			np.strcpy(th)
			np.p++
		case pgNumMI:
			switch {
			case np.sign == '-':
				np.put(np.p, '-')
			case d.is(pgNumFFillMode):
				continue
			default:
				np.put(np.p, ' ')
			}
		case pgNumPL:
			switch {
			case np.sign == '+':
				np.put(np.p, '+')
			case d.is(pgNumFFillMode):
				continue
			default:
				np.put(np.p, ' ')
			}
		case pgNumSG:
			np.put(np.p, byte(np.sign))
		default:
			continue
		}
		np.p++
	}
	np.put(np.p, 0)
	return pgNumCut(np.out), nil
}

// pgNumCut is the C string in b: up to its first NUL.
func pgNumCut(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// pgNumLastRelevantDecnum is get_last_relevant_decnum.
func pgNumLastRelevantDecnum(num []byte) int {
	p := strings.IndexByte(string(num), '.')
	if p < 0 {
		return -1
	}
	r := p
	for i := p + 1; i < len(num) && num[i] != 0; i++ {
		if num[i] != '0' {
			r = i
		}
	}
	return r
}

// pgNumFmtFloat is the server's snprintf (src/port/snprintf.c fmtfloat) of "%.*f" or "%.*e", a
// leading '+' with plus: NaN and the infinities spelled out, precision past 350 padded with zeroes.
func pgNumFmtFloat(v float64, conv byte, prec int, plus bool) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	sign := ""
	if math.Signbit(v) {
		sign = "-"
		v = -v
	} else if plus {
		sign = "+"
	}
	if math.IsInf(v, 0) {
		return sign + "Infinity"
	}
	prec = max(prec, 0)
	p := min(prec, 350)
	s := strconv.FormatFloat(v, conv, p, 64)
	if pad := prec - p; pad > 0 {
		zeros := strings.Repeat("0", pad)
		if e := strings.LastIndexByte(s, 'e'); e >= 0 {
			s = s[:e] + zeros + s[e:]
		} else {
			s += zeros
		}
	}
	return sign + s
}

// pgNumOutSci is numeric_out_sci (get_str_from_var_sci).
func pgNumOutSci(n pgNumeric, rscale int) (string, error) {
	switch n.sign {
	case numPInf:
		return "Infinity", nil
	case numNInf:
		return "-Infinity", nil
	case numNaN:
		return "NaN", nil
	}
	rscale = max(rscale, 0)
	x := n.clone()
	x.strip()
	exponent := 0
	if len(x.digits) > 0 {
		exponent = (x.weight+1)*decDigits - (decDigits - (len(strconv.Itoa(int(x.digits[0]))) - 1))
	}
	t := pgNumPowerTenInt(exponent)
	q, err := divVar(x, t, rscale, true)
	if err != nil {
		return "", err
	}
	e := strconv.Itoa(exponent)
	if exponent >= 0 {
		e = "+" + strings.Repeat("0", max(0, 2-len(e))) + e
	} else {
		e = "-" + strings.Repeat("0", max(0, 3-len(e))) + e[1:]
	}
	return q.String() + "e" + e, nil
}

// pgNumPowerTenInt is power_ten_int.
func pgNumPowerTenInt(exp int) pgNumeric {
	r := pgNumeric{digits: []int32{1}}
	if exp < 0 {
		r.dscale = -exp
		r.weight = (exp+1)/decDigits - 1
	} else {
		r.weight = exp / decDigits
	}
	for exp -= r.weight * decDigits; exp > 0; exp-- {
		r.digits[0] *= 10
	}
	return r
}

// pgNumRound is numeric_round.
func pgNumRound(n pgNumeric, scale int) (pgNumeric, error) {
	if n.isSpecial() {
		return n, nil
	}
	scale = min(max(scale, -numericMaxResult), numericMaxResult)
	v := n.clone()
	v.round(scale)
	if scale < 0 {
		v.dscale = 0
	}
	return makeResult(v)
}

// pgNumMul is numeric_mul by a positive finite multiplier.
func pgNumMul(n, by pgNumeric) (pgNumeric, error) {
	if n.isSpecial() {
		return n, nil
	}
	r := mulVar(n, by, n.dscale+by.dscale)
	if r.dscale > 0x3FFF {
		r.round(0x3FFF)
	}
	return makeResult(r)
}

// pgNumOverflow is the picture filled with '#', its point placed, for a number it cannot hold.
func pgNumOverflow(d pgNumDesc) string {
	b := []byte(strings.Repeat("#", d.pre+d.post+1))
	b[d.pre] = '.'
	return string(b)
}

// pgNumNonFinite is the EEEE output for NaN and the infinities.
func pgNumNonFinite(d pgNumDesc) string {
	b := []byte(strings.Repeat("#", d.pre+d.post+6))
	b[0] = ' '
	b[d.pre+1] = '.'
	return string(b)
}

// pgNumIntDigits is int4_to_char's and int8_to_char's numstr from the integer's text.
func pgNumIntDigits(d *pgNumDesc, orgnum string) (string, int, int) {
	sign := int('+')
	if strings.HasPrefix(orgnum, "-") {
		sign = '-'
		orgnum = orgnum[1:]
	}
	preLen := len(orgnum)
	numstr := orgnum
	if d.post > 0 {
		numstr = orgnum + "." + strings.Repeat("0", d.post)
	}
	out := 0
	if preLen < d.pre {
		out = d.pre - preLen
	} else if preLen > d.pre {
		numstr = pgNumOverflow(*d)
	}
	return numstr, out, sign
}

// pgNumSplitSign is the sign and digits of a printed number, and its padding to the picture.
func pgNumSplitSign(d pgNumDesc, orgnum string) (string, int, int) {
	sign := int('+')
	numstr := orgnum
	if strings.HasPrefix(orgnum, "-") {
		sign = '-'
		numstr = orgnum[1:]
	}
	preLen := len(numstr)
	if p := strings.IndexByte(numstr, '.'); p >= 0 {
		preLen = p
	}
	out := 0
	if preLen < d.pre {
		out = d.pre - preLen
	} else if preLen > d.pre {
		numstr = pgNumOverflow(d)
	}
	return numstr, out, sign
}

// pgNumPrepare is NUM_TOCHAR_prepare: nil nodes for a picture that prints as the empty string.
func pgNumPrepare(f string) ([]pgNumNode, pgNumDesc, bool, error) {
	if len(f) == 0 || len(f) >= (math.MaxInt32-4)/8 {
		return nil, pgNumDesc{}, false, nil
	}
	nodes, d, err := pgNumParse(f)
	return nodes, d, true, err
}

// pgNumericToChar is numeric_to_char.
func pgNumericToChar(value pgNumeric, f string) (any, error) {
	nodes, d, ok, err := pgNumPrepare(f)
	if !ok || err != nil {
		return "", err
	}
	var numstr string
	outPre, sign := 0, 0
	switch {
	case d.is(pgNumFRoman):
		x, err := pgNumRound(value, 0)
		if err != nil {
			return nil, err
		}
		switch {
		case x.isNaN():
			return nil, pgErrorf("cannot convert NaN to integer")
		case x.isInf():
			return nil, pgErrorf("cannot convert infinity to integer")
		}
		v, ok := x.toInt64()
		if !ok || v < math.MinInt32 || v > math.MaxInt32 {
			return nil, errIntOutOfRange
		}
		numstr = pgNumIntToRoman(v)
	case d.is(pgNumFEEEE):
		orgnum, err := pgNumOutSci(value, d.post)
		if err != nil {
			return nil, err
		}
		switch {
		case orgnum == "NaN" || orgnum == "Infinity" || orgnum == "-Infinity":
			numstr = pgNumNonFinite(d)
		case orgnum[0] != '-':
			numstr = " " + orgnum
		default:
			numstr = orgnum
		}
	default:
		val := value
		if d.is(pgNumFMulti) {
			x, err := numericPower(int64ToNumeric(10), int64ToNumeric(int64(d.multi)))
			if err != nil {
				return nil, err
			}
			if val, err = pgNumMul(value, x.(pgNumeric)); err != nil {
				return nil, err
			}
			d.pre += d.multi
		}
		x, err := pgNumRound(val, d.post)
		if err != nil {
			return nil, err
		}
		numstr, outPre, sign = pgNumSplitSign(d, x.String())
	}
	return pgNumProcess(nodes, d, numstr, outPre, sign)
}

// pgNumInt4ToChar is int4_to_char; arm says how the server's build converts an out-of-range double
// to int32 for "V": saturating on arm64, the integer indefinite value on amd64.
func pgNumInt4ToChar(value int32, f string, arm bool, libm pgLibmFuncs) (any, error) {
	nodes, d, ok, err := pgNumPrepare(f)
	if !ok || err != nil {
		return "", err
	}
	var numstr string
	outPre, sign := 0, 0
	switch {
	case d.is(pgNumFRoman):
		numstr = pgNumIntToRoman(int64(value))
	case d.is(pgNumFEEEE):
		numstr = pgNumFmtFloat(float64(value), 'e', d.post, true)
		if numstr[0] == '+' {
			numstr = " " + numstr[1:]
		}
	default:
		v := value
		if d.is(pgNumFMulti) {
			v *= pgNumDoubleToInt32(libm.Pow(10, float64(d.multi)), arm)
			d.pre += d.multi
		}
		numstr, outPre, sign = pgNumIntDigits(&d, strconv.FormatInt(int64(v), 10))
	}
	return pgNumProcess(nodes, d, numstr, outPre, sign)
}

// pgNumDoubleToInt32 is C's (int32) of a double as the server's build converts it.
func pgNumDoubleToInt32(x float64, arm bool) int32 {
	switch {
	case math.IsNaN(x):
		if arm {
			return 0
		}
		return math.MinInt32
	case x >= 2147483648 || x <= -2147483649:
		if !arm || x < 0 {
			return math.MinInt32
		}
		return math.MaxInt32
	}
	return int32(x)
}

// pgNumInt8ToChar is int8_to_char.
func pgNumInt8ToChar(value int64, f string, libm pgLibmFuncs) (any, error) {
	nodes, d, ok, err := pgNumPrepare(f)
	if !ok || err != nil {
		return "", err
	}
	var numstr string
	outPre, sign := 0, 0
	switch {
	case d.is(pgNumFRoman):
		if value < math.MinInt32 || value > math.MaxInt32 {
			return nil, errIntOutOfRange
		}
		numstr = pgNumIntToRoman(value)
	case d.is(pgNumFEEEE):
		orgnum, err := pgNumOutSci(int64ToNumeric(value), d.post)
		if err != nil {
			return nil, err
		}
		numstr = orgnum
		if orgnum[0] != '-' {
			numstr = " " + orgnum
		}
	default:
		v := value
		if d.is(pgNumFMulti) {
			m := math.RoundToEven(libm.Pow(10, float64(d.multi)))
			if math.IsNaN(m) || !(m >= -9223372036854775808.0 && m < 9223372036854775808.0) {
				return nil, errInt8OutOfRange
			}
			mi := int64(m)
			r := v * mi
			if v != 0 && (r/v != mi || (v == -1 && mi == math.MinInt64) || (mi == -1 && v == math.MinInt64)) {
				return nil, errInt8OutOfRange
			}
			v = r
			d.pre += d.multi
		}
		numstr, outPre, sign = pgNumIntDigits(&d, strconv.FormatInt(v, 10))
	}
	return pgNumProcess(nodes, d, numstr, outPre, sign)
}

// pgNumFloat8ToChar is float8_to_char.
func pgNumFloat8ToChar(value float64, f string, libm pgLibmFuncs) (any, error) {
	nodes, d, ok, err := pgNumPrepare(f)
	if !ok || err != nil {
		return "", err
	}
	var numstr string
	outPre, sign := 0, 0
	switch {
	case d.is(pgNumFRoman):
		r := math.RoundToEven(value)
		n := int64(0)
		if r >= math.MinInt32 && r <= math.MaxInt32 {
			n = int64(r)
		}
		numstr = pgNumIntToRoman(n)
	case d.is(pgNumFEEEE):
		if math.IsNaN(value) || math.IsInf(value, 0) {
			numstr = pgNumNonFinite(d)
		} else {
			numstr = pgNumFmtFloat(value, 'e', d.post, true)
			if numstr[0] == '+' {
				numstr = " " + numstr[1:]
			}
		}
	default:
		val := value
		if d.is(pgNumFMulti) {
			val = value * libm.Pow(10, float64(d.multi))
			d.pre += d.multi
		}
		preLen := len(pgNumFmtFloat(math.Abs(val), 'f', 0, false))
		if preLen >= pgNumDblDig {
			d.post = 0
		} else if preLen+d.post > pgNumDblDig {
			d.post = pgNumDblDig - preLen
		}
		numstr, outPre, sign = pgNumSplitSign(d, pgNumFmtFloat(val, 'f', d.post, false))
	}
	return pgNumProcess(nodes, d, numstr, outPre, sign)
}

func init() {
	registerPg("to_char(numeric,text)", func(a []any) (any, error) {
		return pgNumericToChar(num(a[0]), a[1].(string))
	})
	registerPgEnv("to_char(int4,text)", func(env pgEnv, a []any) (any, error) {
		return pgNumInt4ToChar(i4(a[0]), a[1].(string), env.contract, env.libm)
	})
	registerPgEnv("to_char(int8,text)", func(env pgEnv, a []any) (any, error) {
		return pgNumInt8ToChar(i8(a[0]), a[1].(string), env.libm)
	})
	registerPgEnv("to_char(float8,text)", func(env pgEnv, a []any) (any, error) {
		return pgNumFloat8ToChar(f8(a[0]), a[1].(string), env.libm)
	})
}
