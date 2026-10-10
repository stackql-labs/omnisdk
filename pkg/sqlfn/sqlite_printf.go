package sqlfn

import (
	"errors"
	"math"
	"math/bits"
)

// SQLite's number text and printf, ported from SQLite 3.53's util.c and printf.c: the decimal
// conversion of a double (sqlite3FpDecode over SQLite's own power-of-ten tables, which rounds twice
// and so is not Go's exact rounding), the parse of text to a double (sqlite3AtoF), and the printf
// engine behind printf() and format(), REAL-to-TEXT ("%!.17g") and JSON numbers ("%!0.17g").

func multiply128(a, b uint64) (hi, lo uint64) { return bits.Mul64(a, b) }

// multiply160 is the upper 96 bits of ((a<<32)+aLo)*b: the top 64 returned, the next 32 in lo.
func multiply160(a uint64, aLo uint32, b uint64) (uint64, uint32) {
	hi, lo := bits.Mul64(a, b)
	h2, l2 := bits.Mul64(uint64(aLo), b)
	add := h2<<32 | l2>>32
	var carry uint64
	lo, carry = bits.Add64(lo, add, 0)
	hi += carry
	return hi, uint32(lo >> 32)
}

var fpBase = [...]uint64{
	0x8000000000000000, 0xa000000000000000, 0xc800000000000000, 0xfa00000000000000,
	0x9c40000000000000, 0xc350000000000000, 0xf424000000000000, 0x9896800000000000,
	0xbebc200000000000, 0xee6b280000000000, 0x9502f90000000000, 0xba43b74000000000,
	0xe8d4a51000000000, 0x9184e72a00000000, 0xb5e620f480000000, 0xe35fa931a0000000,
	0x8e1bc9bf04000000, 0xb1a2bc2ec5000000, 0xde0b6b3a76400000, 0x8ac7230489e80000,
	0xad78ebc5ac620000, 0xd8d726b7177a8000, 0x878678326eac9000, 0xa968163f0a57b400,
	0xd3c21bcecceda100, 0x84595161401484a0, 0xa56fa5b99019a5c8,
}

var fpScale = [...]uint64{
	0x8049a4ac0c5811ae, 0xcf42894a5dce35ea, 0xa76c582338ed2621, 0x873e4f75e2224e68,
	0xda7f5bf590966848, 0xb080392cc4349dec, 0x8e938662882af53e, 0xe65829b3046b0afa,
	0xba121a4650e4ddeb, 0x964e858c91ba2655, 0xf2d56790ab41c2a2, 0xc428d05aa4751e4c,
	0x9e74d1b791e07e48, 0xcccccccccccccccc, 0xcecb8f27f4200f3a, 0xa70c3c40a64e6c51,
	0x86f0ac99b4e8dafd, 0xda01ee641a708de9, 0xb01ae745b101e9e4, 0x8e41ade9fbebc27d,
	0xe5d3ef282a242e81, 0xb9a74a0637ce2ee1, 0x95f83d0a1fb69cd9, 0xf24a01a73cf2dccf,
	0xc3b8358109e84f07, 0x9e19db92b4e31ba9,
}

var fpScaleLo = [...]uint32{
	0x205b896d, 0x52064cad, 0xaf2af2b8, 0x5a7744a7, 0xaf39a475, 0xbd8d794e, 0x547eb47b,
	0x0cb4a5a3, 0x92f34d62, 0x3a6a07f9, 0xfae27299, 0xaa97e14c, 0x775ea265, 0xcccccccc,
	0x00000000, 0x999090b6, 0x69a028bb, 0xe80e6f48, 0x5ec05dd0, 0x14588f14, 0x8f1668c9,
	0x6d953e2c, 0x4abdaf10, 0xbc633b39, 0x0a862f81, 0x6c07a2c2,
}

const (
	powersOf10First = -348
	powersOf10Last  = 347
)

// powerOfTen is the top 64 bits of 10^p, and the next 32 in lo.
func powerOfTen(p int) (uint64, uint32) {
	var g, n int
	switch {
	case p < 0:
		if p == -1 {
			return fpScale[13], fpScaleLo[13]
		}
		g, n = p/27, p%27
		if n != 0 {
			g--
			n += 27
		}
	case p < 27:
		return fpBase[p], 0
	default:
		g, n = p/27, p%27
	}
	s := fpScale[g+13]
	if n == 0 {
		return s, fpScaleLo[g+13]
	}
	x, lo := multiply160(s, fpScaleLo[g+13], fpBase[n])
	if x&(1<<63) == 0 {
		x = x<<1 | uint64((lo>>31)&1)
		lo = lo<<1 | 1
	}
	return x, lo
}

func pwr10to2(p int) int { return (p * 108853) >> 15 }
func pwr2to10(p int) int { return (p * 78913) >> 18 }

// fp2Convert10 is m*2^e as approximately d*10^p with at least n significant digits.
func fp2Convert10(m uint64, e int, n int) (uint64, int) {
	p := n - 1 - pwr2to10(e+63)
	pw, _ := powerOfTen(p)
	h, _ := multiply128(m, pw)
	if n == 18 {
		h >>= uint(-(e + pwr10to2(p) + 2))
		return (h + ((h << 1) & 2)) >> 1, -p
	}
	return h >> uint(-(e + pwr10to2(p) + 1)), -p
}

// fp10Convert2 is the double nearest d*10^p.
func fp10Convert2(d uint64, p int) float64 {
	if p < powersOf10First {
		return 0
	}
	if p > powersOf10Last {
		return math.Inf(1)
	}
	b := 64 - bits.LeadingZeros64(d)
	lp := pwr10to2(p)
	e := 53 - b - lp
	if e > 1074 {
		if e >= 1130 {
			return 0
		}
		e = 1074
	}
	s := -(e - (64 - b) + lp + 3)
	pwr10h, pwr10l := powerOfTen(p)
	if pwr10l != 0 {
		pwr10h++
		pwr10l = ^pwr10l
	}
	x := d << uint(64-b)
	hi, lo := multiply128(x, pwr10h)
	mid1 := uint32(lo >> 32)
	sticky := uint64(1)
	if hi&(uint64(1)<<uint(s)-1) == 0 {
		h2, _ := multiply128(x, uint64(pwr10l)<<32)
		mid2 := uint32(h2 >> 32)
		if mid1-mid2 > 1 {
			sticky = 1
		} else {
			sticky = 0
		}
		if mid1 < mid2 {
			hi--
		}
	}
	u := (hi >> uint(s)) | sticky
	adj := 0
	if u >= uint64(1)<<55-2 {
		adj = 1
	}
	if adj != 0 {
		u = (u >> 1) | (u & 1)
		e -= adj
	}
	m := (u + 1 + ((u >> 2) & 1)) >> 2
	if e <= -972 {
		return math.Inf(1)
	}
	if m&(uint64(1)<<52) != 0 {
		m = (m &^ (uint64(1) << 52)) | uint64(1075-e)<<52
	}
	return math.Float64frombits(m)
}

// fpDecode is a double's significant decimal digits (z), the decimal point's position (iDP), its
// sign, and whether it is an infinity (1) or NaN (2).
type fpDecode struct {
	z         []byte
	iDP       int
	sign      byte
	isSpecial int
}

// decodeFloat is sqlite3FpDecode: r rounded to iRound significant digits (iRound <= 0: to -iRound
// places after the point), at most mxRound.
func decodeFloat(r float64, iRound, mxRound int) fpDecode {
	var p fpDecode
	if r < 0 {
		p.sign = '-'
		r = -r
	} else if r == 0 {
		p.sign = '+'
		p.z = []byte{'0'}
		p.iDP = 1
		return p
	} else {
		p.sign = '+'
	}
	v := math.Float64bits(r)
	e := int((v >> 52) & 0x7ff)
	if e == 0x7ff {
		p.isSpecial = 1
		if v != 0x7ff0000000000000 {
			p.isSpecial = 2
		}
		return p
	}
	v &= 0x000fffffffffffff
	if e == 0 {
		nn := bits.LeadingZeros64(v)
		v <<= uint(nn)
		e = -1074 - nn
	} else {
		v = v<<11 | 1<<63
		e -= 1086
	}
	nd := iRound + 1
	if iRound <= 0 || iRound >= 18 {
		nd = 18
	}
	var exp int
	v, exp = fp2Convert10(v, e, nd)

	const digitsLen = 20
	var buf [digitsLen + 8]byte // spare zeros past the digits, as C reads past them
	const base = 1              // one slot before the digits for a carry
	i := base + digitsLen
	for v > 0 {
		i--
		buf[i] = byte(v%10) + '0'
		v /= 10
	}
	n := base + digitsLen - i
	p.iDP = n + exp
	if iRound <= 0 {
		iRound = p.iDP - iRound
		if iRound == 0 && buf[i] >= '5' {
			iRound = 1
			i--
			buf[i] = '0'
			n++
			p.iDP++
		}
	}
	z := buf[i:]
	zi := i
	if iRound > 0 && (iRound < n || n > mxRound) {
		if iRound > mxRound {
			iRound = mxRound
		}
		if iRound == 17 {
			// At exactly 17 (only "%!.17g"), use fewer digits where they read back as r.
			if z[15] == '9' && z[14] == '9' {
				jj := 14
				for jj > 0 && z[jj-1] == '9' {
					jj--
				}
				var v2 uint64
				if jj == 0 {
					v2 = 1
				} else {
					v2 = uint64(z[0] - '0')
					for kk := 1; kk < jj; kk++ {
						v2 = v2*10 + uint64(z[kk]-'0')
					}
					v2++
				}
				if r == fp10Convert2(v2, exp+n-jj) {
					iRound = jj + 1
				}
			} else if p.iDP >= n || (z[15] == '0' && z[14] == '0' && z[13] == '0') {
				jj := 13
				for z[jj-1] == '0' {
					jj--
				}
				v2 := uint64(z[0] - '0')
				for kk := 1; kk < jj; kk++ {
					v2 = v2*10 + uint64(z[kk]-'0')
				}
				if r == fp10Convert2(v2, exp+n-jj) {
					iRound = jj + 1
				}
			}
		}
		n = iRound
		if z[iRound] >= '5' {
			j := iRound - 1
			for {
				z[j]++
				if z[j] <= '9' {
					break
				}
				z[j] = '0'
				if j == 0 {
					zi--
					buf[zi] = '1'
					z = buf[zi:]
					n++
					p.iDP++
					break
				}
				j--
			}
		}
	}
	for z[n-1] == '0' {
		n--
	}
	p.z = append([]byte(nil), z[:n]...)
	return p
}

// sqliteAtoFRaw is sqlite3AtoF: the value of the number text begins with, and a state that is
// positive only when the whole text (less surrounding whitespace) is the number.
func sqliteAtoFRaw(z string) (float64, int) {
	const bound = (math.MaxUint64 - 9) / 10
	i := 0
	neg := false
	var s uint64
	d := 0
	mState := 0
	for {
		c := at(z, i)
		if isDigitByte(c) {
			break
		}
		if c == '-' || c == '+' {
			neg = c == '-'
			i++
			if isDigitByte(at(z, i)) {
				break
			}
			goto fraction
		}
		if isSpaceByte(c) {
			for isSpaceByte(at(z, i)) {
				i++
			}
			continue
		}
		goto fraction
	}
	// The integer part.
	mState = 1
	s = uint64(at(z, i) - '0')
	i++
	for isDigitByte(at(z, i)) {
		s = s*10 + uint64(at(z, i)-'0')
		i++
		if s >= bound {
			mState = 9
			for isDigitByte(at(z, i)) {
				i++
				d++
			}
			break
		}
	}
fraction:
	if at(z, i) == '.' {
		i++
		if isDigitByte(at(z, i)) {
			mState |= 1
			for {
				if s < bound {
					s = s*10 + uint64(at(z, i)-'0')
					d--
				} else {
					mState = 11
				}
				i++
				if !isDigitByte(at(z, i)) {
					break
				}
			}
		} else if mState == 0 {
			return 0, 0
		}
		mState |= 2
	} else if mState == 0 {
		return 0, 0
	}
	if c := at(z, i); c == 'e' || c == 'E' {
		i++
		esign := 1
		if at(z, i) == '-' {
			esign = -1
			i++
		} else if at(z, i) == '+' {
			i++
		}
		if isDigitByte(at(z, i)) {
			exp := int(at(z, i) - '0')
			i++
			mState |= 2
			for isDigitByte(at(z, i)) {
				if exp < 10000 {
					exp = exp*10 + int(at(z, i)-'0')
				} else {
					exp = 10000
				}
				i++
			}
			d += esign * exp
		} else {
			i--
		}
	}
	var r float64
	if s == 0 {
		mState |= 4
	} else {
		r = fp10Convert2(s, d)
	}
	if neg {
		r = -r
	}
	if at(z, i) == 0 {
		return r, mState
	}
	if isSpaceByte(at(z, i)) {
		for isSpaceByte(at(z, i)) {
			i++
		}
		if at(z, i) == 0 {
			return r, mState
		}
	}
	return r, -16 | mState
}

// printf

const (
	etRadix = iota + 1
	etFloat
	etExp
	etGeneric
	etSize
	etString
	etDynString
	etPercent
	etCharX
	etEscapeQ
	etEscapeQQ
	etToken
	etSrcItem
	etPointer
	etEscapeW
	etOrdinal
	etDecimal
	etInvalid
)

const (
	aDigits = "0123456789ABCDEF0123456789abcdef"
	aPrefix = "-x0\x00X0\x00" // with C's terminating NUL
)

type etInfo struct {
	fmttype byte
	base    byte
	flags   byte
	typ     int
	charset int
	prefix  int
}

var fmtInfo = map[byte]etInfo{
	's': {'s', 0, 4, etString, 0, 0}, 'E': {'E', 0, 1, etExp, 14, 0}, 'u': {'u', 10, 0, etDecimal, 0, 0},
	'G': {'G', 0, 1, etGeneric, 14, 0}, 'w': {'w', 0, 4, etEscapeW, 0, 0}, 'x': {'x', 16, 0, etRadix, 16, 1},
	'c': {'c', 0, 0, etCharX, 0, 0}, 'z': {'z', 0, 4, etDynString, 0, 0}, 'd': {'d', 10, 1, etDecimal, 0, 0},
	'e': {'e', 0, 1, etExp, 30, 0}, 'f': {'f', 0, 1, etFloat, 0, 0}, 'g': {'g', 0, 1, etGeneric, 30, 0},
	'Q': {'Q', 0, 4, etEscapeQQ, 0, 0}, 'i': {'i', 10, 1, etDecimal, 0, 0}, '%': {'%', 0, 0, etPercent, 0, 0},
	'T': {'T', 0, 0, etToken, 0, 0}, 'S': {'S', 0, 0, etSrcItem, 0, 0}, 'X': {'X', 16, 0, etRadix, 0, 4},
	'n': {'n', 0, 0, etSize, 0, 0}, 'o': {'o', 8, 0, etRadix, 0, 2}, 'p': {'p', 16, 0, etPointer, 0, 1},
	'q': {'q', 0, 4, etEscapeQ, 0, 0}, 'r': {'r', 10, 1, etOrdinal, 0, 0},
}

const (
	sqliteMaxLength  = 1000000000
	fpPrecisionLimit = 100000000
	etBufSize        = 70
	errTooBigMessage = "string or blob too big"
)

var errTooBig = errors.New(errTooBigMessage)

// strAccum is SQLite's string accumulator, capped at SQLite's maximum length.
type strAccum struct {
	b      []byte
	max    int // mxAlloc; 0 for a fixed-size buffer that truncates
	tooBig bool
	grown  bool // any append, even of nothing, allocates; one that never grew finishes as NULL
}

func (a *strAccum) append(s []byte) {
	a.grown = true
	if a.tooBig {
		return
	}
	if a.max > 0 && len(a.b)+len(s) > a.max {
		a.tooBig = true
		a.b = nil
		return
	}
	a.b = append(a.b, s...)
}

func (a *strAccum) appendChar(n int, c byte) {
	if n <= 0 || a.tooBig {
		return
	}
	if a.max > 0 && len(a.b)+n > a.max {
		a.tooBig = true
		a.b = nil
		return
	}
	for k := 0; k < n; k++ {
		a.b = append(a.b, c)
	}
}

// printfArgs are SQL values consumed by a format, in order.
type printfArgs struct {
	args []any
	used int
}

func (p *printfArgs) next() (any, bool) {
	if p.used >= len(p.args) {
		return nil, false
	}
	v := p.args[p.used]
	p.used++
	return v, true
}

func (p *printfArgs) intArg() int64 {
	v, ok := p.next()
	if !ok {
		return 0
	}
	return sqlInt(v)
}

func (p *printfArgs) doubleArg() float64 {
	v, ok := p.next()
	if !ok {
		return 0
	}
	return sqlReal(v)
}

// textArg is the next argument as C text; nil for NULL or no argument.
func (p *printfArgs) textArg() []byte {
	v, ok := p.next()
	if !ok || isNull(v) {
		return nil
	}
	return []byte(beforeNUL(sqlText(v)))
}

// sqlitePrintf renders fmt with args as SQLite's sqlite3_str_vappendf does with SQL arguments.
func sqlitePrintf(acc *strAccum, fmt string, args *printfArgs) {
	f := 0
	for f < len(fmt) && fmt[f] != 0 {
		c := fmt[f]
		if c != '%' {
			start := f
			for f < len(fmt) && fmt[f] != '%' && fmt[f] != 0 {
				f++
			}
			acc.append([]byte(fmt[start:f]))
			if at(fmt, f) == 0 {
				return
			}
		}
		f++
		c = at(fmt, f)
		if c == 0 {
			acc.append([]byte("%"))
			return
		}
		var leftJustify, alternateForm, altForm2, zeroPad bool
		var flagPrefix, cThousand byte
		width := 0
		precision := -1
		for done := false; !done; {
			switch c {
			case '-':
				leftJustify = true
			case '+':
				flagPrefix = '+'
			case ' ':
				flagPrefix = ' '
			case '#':
				alternateForm = true
			case '!':
				altForm2 = true
			case '0':
				zeroPad = true
			case ',':
				cThousand = ','
			case 'l':
				f++
				c = at(fmt, f)
				if c == 'l' {
					f++
					c = at(fmt, f)
				}
				done = true
			case '1', '2', '3', '4', '5', '6', '7', '8', '9':
				wx := uint32(c - '0')
				for {
					f++
					c = at(fmt, f)
					if c < '0' || c > '9' {
						break
					}
					wx = wx*10 + uint32(c-'0')
				}
				width = int(wx & 0x7fffffff)
				if c != '.' && c != 'l' {
					done = true
				} else {
					f--
				}
			case '*':
				width = int(int32(args.intArg()))
				if width < 0 {
					leftJustify = true
					if width >= -2147483647 {
						width = -width
					} else {
						width = 0
					}
				}
				if c = at(fmt, f+1); c != '.' && c != 'l' {
					f++
					c = at(fmt, f)
					done = true
				}
			case '.':
				f++
				c = at(fmt, f)
				if c == '*' {
					precision = int(int32(args.intArg()))
					if precision < 0 {
						if precision >= -2147483647 {
							precision = -precision
						} else {
							precision = -1
						}
					}
					f++
					c = at(fmt, f)
				} else {
					var px uint32
					for c >= '0' && c <= '9' {
						px = px*10 + uint32(c-'0')
						f++
						c = at(fmt, f)
					}
					precision = int(px & 0x7fffffff)
				}
				if c == 'l' {
					f--
				} else {
					done = true
				}
			default:
				done = true
			}
			if !done {
				f++
				c = at(fmt, f)
				if c == 0 {
					break
				}
			}
		}
		info, known := fmtInfo[c]
		xtype := etInvalid
		if known {
			xtype = info.typ
		}
		var out []byte // the conversion's text, before width padding
		switch xtype {
		case etPointer, etOrdinal, etRadix, etDecimal:
			if xtype != etDecimal {
				cThousand = 0
			}
			var longvalue uint64
			var prefix byte
			if info.flags&1 != 0 {
				v := args.intArg()
				if v < 0 {
					longvalue = uint64(^v) + 1
					prefix = '-'
				} else {
					longvalue = uint64(v)
					prefix = flagPrefix
				}
			} else {
				longvalue = uint64(args.intArg())
			}
			if longvalue == 0 {
				alternateForm = false
			}
			hasPrefix := 0
			if prefix != 0 {
				hasPrefix = 1
			}
			if zeroPad && precision < width-hasPrefix {
				precision = width - hasPrefix
			}
			if precision >= etBufSize-10-etBufSize/3 {
				n := uint64(precision) + 10
				if cThousand != 0 {
					n += uint64(precision / 3)
				}
				if n > sqliteMaxLength {
					acc.tooBig = true
					return
				}
			}
			var digits []byte
			if xtype == etOrdinal {
				zOrd := "thstndrd"
				x := int(longvalue % 10)
				if x >= 4 || (longvalue/10)%10 == 1 {
					x = 0
				}
				digits = append(digits, zOrd[x*2+1], zOrd[x*2])
			}
			cset := aDigits[info.charset:]
			base := uint64(info.base)
			for {
				digits = append(digits, cset[longvalue%base])
				longvalue /= base
				if longvalue == 0 {
					break
				}
			}
			length := len(digits)
			for length < precision {
				digits = append(digits, '0')
				length++
			}
			// digits is reversed; build forward.
			fwd := make([]byte, len(digits))
			for k := range digits {
				fwd[len(digits)-1-k] = digits[k]
			}
			if cThousand != 0 {
				nn := (length - 1) / 3
				ix := (length-1)%3 + 1
				var t []byte
				for k := 0; k < len(fwd); k++ {
					t = append(t, fwd[k])
					ix--
					if ix == 0 && nn > 0 {
						t = append(t, cThousand)
						nn--
						ix = 3
					}
				}
				fwd = t
			}
			var pre []byte
			if prefix != 0 {
				pre = append(pre, prefix)
			}
			if alternateForm && info.prefix != 0 {
				// aPrefix is written backwards before the digits.
				var px []byte
				for k := info.prefix; aPrefix[k] != 0; k++ {
					px = append([]byte{aPrefix[k]}, px...)
				}
				pre = append(px, pre...)
			}
			out = append(pre, fwd...)
		case etFloat, etExp, etGeneric:
			realvalue := args.doubleArg()
			if precision < 0 {
				precision = 6
			}
			if precision > fpPrecisionLimit {
				precision = fpPrecisionLimit
			}
			var iRound int
			switch xtype {
			case etFloat:
				iRound = -precision
			case etGeneric:
				if precision == 0 {
					precision = 1
				}
				iRound = precision
			default:
				iRound = precision + 1
			}
			mx := 16
			if altForm2 {
				mx = 20
			}
			s := decodeFloat(realvalue, iRound, mx)
			if s.isSpecial != 0 {
				if s.isSpecial == 2 {
					if zeroPad {
						out = []byte("null")
					} else {
						out = []byte("NaN")
					}
					break
				} else if zeroPad {
					s.z = []byte{'9'}
					s.iDP = 1000
				} else {
					b := []byte("-Inf")
					if s.sign == '-' {
					} else if flagPrefix != 0 {
						b[0] = flagPrefix
					} else {
						b = b[1:]
					}
					out = b
					break
				}
			}
			var prefix byte
			if s.sign == '-' {
				if alternateForm && flagPrefix == 0 && xtype == etFloat && s.iDP <= iRound {
					prefix = 0
				} else {
					prefix = '-'
				}
			} else {
				prefix = flagPrefix
			}
			exp := s.iDP - 1
			var rtz bool
			if xtype == etGeneric {
				precision--
				rtz = !alternateForm
				if exp < -4 || exp > precision {
					xtype = etExp
				} else {
					precision = precision - exp
					xtype = etFloat
				}
			} else {
				rtz = altForm2
			}
			e2 := 0
			if xtype != etExp {
				e2 = s.iDP - 1
			}
			need := int64(max(e2, 0)) + int64(precision) + int64(width) + 10
			if cThousand != 0 && e2 > 0 {
				need += int64((e2 + 2) / 3)
			}
			if need+int64(len(acc.b)) > sqliteMaxLength && acc.max > 0 {
				acc.tooBig = true
				return
			}
			var b []byte
			dp := precision > 0 || alternateForm || altForm2
			if prefix != 0 {
				b = append(b, prefix)
			}
			j := 0
			if e2 < 0 {
				b = append(b, '0')
			} else if cThousand != 0 {
				for ; e2 >= 0; e2-- {
					if j < len(s.z) {
						b = append(b, s.z[j])
						j++
					} else {
						b = append(b, '0')
					}
					if e2%3 == 0 && e2 > 1 {
						b = append(b, ',')
					}
				}
			} else {
				j = e2 + 1
				if j > len(s.z) {
					j = len(s.z)
				}
				b = append(b, s.z[:j]...)
				e2 -= j
				if e2 >= 0 {
					for k := 0; k <= e2; k++ {
						b = append(b, '0')
					}
					e2 = -1
				}
			}
			if dp {
				b = append(b, '.')
			}
			if e2 < -1 && precision > 0 {
				nn := -1 - e2
				if nn > precision {
					nn = precision
				}
				for k := 0; k < nn; k++ {
					b = append(b, '0')
				}
				precision -= nn
			}
			if precision > 0 {
				nn := len(s.z) - j
				if nn > precision {
					nn = precision
				}
				if nn > 0 {
					b = append(b, s.z[j:j+nn]...)
					precision -= nn
				}
				if precision > 0 && !rtz {
					for k := 0; k < precision; k++ {
						b = append(b, '0')
					}
				}
			}
			if rtz && dp {
				for b[len(b)-1] == '0' {
					b = b[:len(b)-1]
				}
				if b[len(b)-1] == '.' {
					if altForm2 {
						b = append(b, '0')
					} else {
						b = b[:len(b)-1]
					}
				}
			}
			if xtype == etExp {
				exp = s.iDP - 1
				b = append(b, aDigits[info.charset])
				if exp < 0 {
					b = append(b, '-')
					exp = -exp
				} else {
					b = append(b, '+')
				}
				if exp >= 100 {
					b = append(b, byte(exp/100)+'0')
					exp %= 100
				}
				b = append(b, byte(exp/10)+'0', byte(exp%10)+'0')
			}
			if len(b) < width {
				nPad := width - len(b)
				switch {
				case leftJustify:
					for k := 0; k < nPad; k++ {
						b = append(b, ' ')
					}
				case !zeroPad:
					b = append(make([]byte, nPad), b...)
					for k := 0; k < nPad; k++ {
						b[k] = ' '
					}
				default:
					adj := 0
					if prefix != 0 {
						adj = 1
					}
					pad := make([]byte, nPad)
					for k := range pad {
						pad[k] = '0'
					}
					b = append(append(append([]byte(nil), b[:adj]...), pad...), b[adj:]...)
				}
			}
			acc.append(b)
			f++
			continue
		case etSize:
			width = 0
		case etPercent:
			out = []byte("%")
		case etCharX:
			bufpt := args.textArg()
			var buf []byte
			if bufpt != nil {
				c := at(string(bufpt), 0) // the C string's first byte, its NUL when empty
				buf = append(buf, c)
				if c&0xc0 == 0xc0 {
					for k := 1; len(buf) < 4 && k < len(bufpt) && bufpt[k]&0xc0 == 0x80; k++ {
						buf = append(buf, bufpt[k])
					}
				}
			} else {
				buf = []byte{0}
			}
			if precision > 1 {
				width -= precision - 1
				if width > 1 && !leftJustify {
					acc.appendChar(width-1, ' ')
					width = 0
				}
				for k := 0; k < precision-1; k++ {
					acc.append(buf)
				}
			}
			out = buf
			altForm2 = true
			width = adjustWidthForUTF8(out, width, altForm2)
		case etString, etDynString:
			bufpt := args.textArg()
			length := len(bufpt)
			if precision >= 0 {
				if altForm2 {
					k := 0
					for p := precision; p > 0 && k < len(bufpt); p-- {
						k = skipUTF8(bufpt, k)
					}
					length = k
				} else if precision < length {
					length = precision
				}
			}
			out = bufpt[:length]
			width = adjustWidthForUTF8(out, width, altForm2)
		case etEscapeQ, etEscapeQQ, etEscapeW:
			escarg := args.textArg()
			needQuote := 0
			if escarg == nil {
				if xtype == etEscapeQQ {
					escarg = []byte("NULL")
				} else {
					escarg = []byte("(NULL)")
				}
			} else if xtype == etEscapeQQ {
				needQuote = 1
			}
			q := byte('\'')
			if xtype == etEscapeW {
				q = '"'
				alternateForm = false
			}
			i := 0
			for k := precision; k != 0 && i < len(escarg); i, k = i+1, k-1 {
				if altForm2 && escarg[i]&0xc0 == 0xc0 {
					for i+1 < len(escarg) && escarg[i+1]&0xc0 == 0x80 {
						i++
					}
				}
			}
			if alternateForm {
				nCtrl := 0
				for k := 0; k < i; k++ {
					if escarg[k] != '\\' && escarg[k] <= 0x1f {
						nCtrl++
					}
				}
				if nCtrl != 0 || xtype == etEscapeQ {
					if xtype == etEscapeQQ {
						needQuote = 2
					}
				} else {
					alternateForm = false
				}
			}
			var b []byte
			switch needQuote {
			case 2:
				b = append(b, "unistr('"...)
			case 1:
				b = append(b, '\'')
			}
			for k := 0; k < i; k++ {
				ch := escarg[k]
				b = append(b, ch)
				if alternateForm {
					switch {
					case ch == q:
						b = append(b, ch)
					case ch == '\\':
						b = append(b, '\\')
					case ch <= 0x1f:
						b[len(b)-1] = '\\'
						hi := byte('0')
						if ch >= 0x10 {
							hi = '1'
						}
						b = append(b, 'u', '0', '0', hi, "0123456789abcdef"[ch&0xf])
					}
				} else if ch == q {
					b = append(b, ch)
				}
			}
			if needQuote != 0 {
				b = append(b, '\'')
				if needQuote == 2 {
					b = append(b, ')')
				}
			}
			out = b
			width = adjustWidthForUTF8(out, width, altForm2)
		default:
			// %T, %S and unknown conversions end the output.
			return
		}
		if width -= len(out); width > 0 {
			if !leftJustify {
				acc.appendChar(width, ' ')
			}
			acc.append(out)
			if leftJustify {
				acc.appendChar(width, ' ')
			}
		} else {
			acc.append(out)
		}
		f++
	}
}

// adjustWidthForUTF8 widens a field by its UTF-8 continuation bytes when widths count characters.
func adjustWidthForUTF8(b []byte, width int, altForm2 bool) int {
	if altForm2 && width > 0 {
		for _, c := range b {
			if c&0xc0 == 0x80 {
				width++
			}
		}
	}
	return width
}

// skipUTF8 is SQLITE_SKIP_UTF8: past one character at k.
func skipUTF8(z []byte, k int) int {
	c := z[k]
	k++
	if c >= 0xc0 {
		for k < len(z) && z[k]&0xc0 == 0x80 {
			k++
		}
	}
	return k
}

// formatFloat is one float conversion, as SQLite's REAL text and JSON numbers use it.
func formatFloat(format string, f float64) string {
	acc := &strAccum{}
	sqlitePrintf(acc, format, &printfArgs{args: []any{f}})
	return string(acc.b)
}

func sqlitePrintfFunc(a []any) (any, error) {
	if len(a) < 1 || isNull(a[0]) {
		return nil, nil
	}
	acc := &strAccum{max: sqliteMaxLength}
	sqlitePrintf(acc, beforeNUL(sqlText(a[0])), &printfArgs{args: a[1:]})
	if acc.tooBig {
		return nil, errTooBig
	}
	if !acc.grown {
		return nil, nil
	}
	return string(acc.b), nil
}

func sqlitePrintfFuncs() []Func {
	return []Func{
		NewScalar("printf", 0, -1, sqlitePrintfFunc),
		NewScalar("format", 0, -1, sqlitePrintfFunc),
	}
}
