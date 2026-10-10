package sqlfn

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Postgres numeric, ported from Postgres 14's numeric.c: base-10000 digits with a weight, a sign and
// a display scale, and each operation by the same algorithm Postgres uses — including its truncated
// multiplication and its floating-point-estimated division in the transcendental functions — so a
// result agrees with Postgres's to its last digit and its display scale. C's 32-bit int arithmetic,
// overflow included, is int32 here.

const (
	nbase              = 10000
	halfNbase          = 5000
	decDigits          = 4
	mulGuardDigits     = 2
	divGuardDigits     = 4
	numericMaxDisplay  = 1000
	numericMinDisplay  = 0
	numericMaxResult   = 2000
	numericMinSigDigit = 16

	numPos  = 0x0000
	numNeg  = 0x4000
	numNaN  = 0xC000
	numPInf = 0xD000
	numNInf = 0xF000
)

var roundPowers = [4]int32{0, 1000, 100, 10}

// pgNumeric is a numeric value: digits[0] has weight `weight` (in base-10000 places), dscale is the
// number of decimal digits displayed after the point, and sign is numPos, numNeg, numNaN, numPInf or
// numNInf.
type pgNumeric struct {
	digits []int32
	weight int
	sign   int
	dscale int
}

var (
	numZero    = pgNumeric{}
	numOne     = pgNumeric{digits: []int32{1}}
	numMinus1  = pgNumeric{digits: []int32{1}, sign: numNeg}
	numTwo     = pgNumeric{digits: []int32{2}}
	numPoint9  = pgNumeric{digits: []int32{9000}, weight: -1, dscale: 1}
	numOnePt1  = pgNumeric{digits: []int32{1, 1000}, dscale: 1}
	numNaNVal  = pgNumeric{sign: numNaN}
	numPInfVal = pgNumeric{sign: numPInf}
	numNInfVal = pgNumeric{sign: numNInf}
)

func (n pgNumeric) isSpecial() bool { return n.sign&0xC000 == 0xC000 }
func (n pgNumeric) isNaN() bool     { return n.sign == numNaN }
func (n pgNumeric) isInf() bool     { return n.sign == numPInf || n.sign == numNInf }
func (n pgNumeric) ndigits() int    { return len(n.digits) }

func (n pgNumeric) clone() pgNumeric {
	n.digits = append([]int32(nil), n.digits...)
	return n
}

var (
	errNumericOverflow = pgErrorf("value overflows numeric format")
	errDivisionByZero  = pgErrorf("division by zero")
)

func invalidNumeric(s string) error {
	return pgErrorf("invalid input syntax for type numeric: %q", s)
}

// parseNumeric is numeric_in.
func parseNumeric(str string) (pgNumeric, error) {
	cp := 0
	for cp < len(str) && isPgSpace(str[cp]) {
		cp++
	}
	rest := str[cp:]
	var res pgNumeric
	special := false
	for _, s := range []struct {
		text string
		v    pgNumeric
	}{
		{"NaN", numNaNVal}, {"Infinity", numPInfVal}, {"+Infinity", numPInfVal}, {"-Infinity", numNInfVal},
		{"inf", numPInfVal}, {"+inf", numPInfVal}, {"-inf", numNInfVal},
	} {
		if len(rest) >= len(s.text) && strings.EqualFold(rest[:len(s.text)], s.text) {
			res, cp, special = s.v, cp+len(s.text), true
			break
		}
	}
	if !special {
		var err error
		res, cp, err = setVarFromStr(str, cp)
		if err != nil {
			return pgNumeric{}, err
		}
	}
	for ; cp < len(str); cp++ {
		if !isPgSpace(str[cp]) {
			return pgNumeric{}, invalidNumeric(str)
		}
	}
	if special {
		return res, nil
	}
	return makeResult(res)
}

func isPgSpace(c byte) bool { return c == ' ' || (c >= '\t' && c <= '\r') }

// setVarFromStr parses a finite number at str[cp:], returning it and where it ended.
func setVarFromStr(str string, cp int) (pgNumeric, int, error) {
	haveDP := false
	sign := numPos
	dweight := -1
	dscale := 0
	switch at(str, cp) {
	case '+':
		cp++
	case '-':
		sign = numNeg
		cp++
	}
	if at(str, cp) == '.' {
		haveDP = true
		cp++
	}
	if !isDigitByte(at(str, cp)) {
		return pgNumeric{}, cp, invalidNumeric(str)
	}
	decdigits := make([]int32, decDigits, len(str)+2*decDigits)
	for cp < len(str) {
		c := str[cp]
		if isDigitByte(c) {
			decdigits = append(decdigits, int32(c-'0'))
			cp++
			if !haveDP {
				dweight++
			} else {
				dscale++
			}
		} else if c == '.' {
			if haveDP {
				return pgNumeric{}, cp, invalidNumeric(str)
			}
			haveDP = true
			cp++
		} else {
			break
		}
	}
	ddigits := len(decdigits) - decDigits
	decdigits = append(decdigits, make([]int32, decDigits-1)...)
	if c := at(str, cp); c == 'e' || c == 'E' {
		cp++
		j := cp
		if c := at(str, j); c == '+' || c == '-' {
			j++
		}
		k := j
		for isDigitByte(at(str, k)) {
			k++
		}
		if k == j {
			return pgNumeric{}, cp, invalidNumeric(str)
		}
		exponent, err := strconv.ParseInt(str[cp:k], 10, 64)
		if err != nil || exponent >= math.MaxInt32/2 || exponent <= -(math.MaxInt32/2) {
			return pgNumeric{}, cp, errNumericOverflow
		}
		cp = k
		dweight += int(exponent)
		dscale -= int(exponent)
		if dscale < 0 {
			dscale = 0
		}
	}
	var weight int
	if dweight >= 0 {
		weight = (dweight+1+decDigits-1)/decDigits - 1
	} else {
		weight = -((-dweight-1)/decDigits + 1)
	}
	offset := (weight+1)*decDigits - (dweight + 1)
	ndigits := (ddigits + offset + decDigits - 1) / decDigits
	res := pgNumeric{digits: make([]int32, ndigits), sign: sign, weight: weight, dscale: dscale}
	i := decDigits - offset
	for d := 0; d < ndigits; d++ {
		res.digits[d] = ((decdigits[i]*10+decdigits[i+1])*10+decdigits[i+2])*10 + decdigits[i+3]
		i += decDigits
	}
	res.strip()
	return res, cp, nil
}

// String is numeric_out.
func (n pgNumeric) String() string {
	switch n.sign {
	case numPInf:
		return "Infinity"
	case numNInf:
		return "-Infinity"
	case numNaN:
		return "NaN"
	}
	var b strings.Builder
	if n.sign == numNeg {
		b.WriteByte('-')
	}
	d := 0
	if n.weight < 0 {
		d = n.weight + 1
		b.WriteByte('0')
	} else {
		for d = 0; d <= n.weight; d++ {
			dig := int32(0)
			if d < len(n.digits) {
				dig = n.digits[d]
			}
			putit := d > 0
			for _, p := range []int32{1000, 100, 10} {
				d1 := dig / p
				dig -= d1 * p
				putit = putit || d1 > 0
				if putit {
					b.WriteByte(byte(d1) + '0')
				}
			}
			b.WriteByte(byte(dig) + '0')
		}
	}
	if n.dscale > 0 {
		b.WriteByte('.')
		var frac strings.Builder
		for i := 0; i < n.dscale; d, i = d+1, i+decDigits {
			dig := int32(0)
			if d >= 0 && d < len(n.digits) {
				dig = n.digits[d]
			}
			for _, p := range []int32{1000, 100, 10} {
				d1 := dig / p
				dig -= d1 * p
				frac.WriteByte(byte(d1) + '0')
			}
			frac.WriteByte(byte(dig) + '0')
		}
		b.WriteString(frac.String()[:n.dscale])
	}
	return b.String()
}

// strip removes leading and trailing zero digits; zero becomes positive with weight 0.
func (n *pgNumeric) strip() {
	d := n.digits
	for len(d) > 0 && d[0] == 0 {
		d = d[1:]
		n.weight--
	}
	for len(d) > 0 && d[len(d)-1] == 0 {
		d = d[:len(d)-1]
	}
	if len(d) == 0 {
		n.sign = numPos
		n.weight = 0
	}
	n.digits = d
}

// makeResult is make_result: the value stripped, or an error if its weight or scale does not fit.
func makeResult(n pgNumeric) (pgNumeric, error) {
	if n.isSpecial() {
		return pgNumeric{sign: n.sign}, nil
	}
	n = n.clone()
	n.strip()
	if n.weight > math.MaxInt16 || n.weight < math.MinInt16 || n.dscale > 0x3FFF || n.dscale < 0 {
		return pgNumeric{}, errNumericOverflow
	}
	return n, nil
}

func cmpAbsCommon(d1 []int32, w1 int, d2 []int32, w2 int) int {
	i1, i2 := 0, 0
	for w1 > w2 && i1 < len(d1) {
		if d1[i1] != 0 {
			return 1
		}
		i1++
		w1--
	}
	for w2 > w1 && i2 < len(d2) {
		if d2[i2] != 0 {
			return -1
		}
		i2++
		w2--
	}
	if w1 == w2 {
		for i1 < len(d1) && i2 < len(d2) {
			stat := d1[i1] - d2[i2]
			i1++
			i2++
			if stat > 0 {
				return 1
			}
			if stat < 0 {
				return -1
			}
		}
	}
	for ; i1 < len(d1); i1++ {
		if d1[i1] != 0 {
			return 1
		}
	}
	for ; i2 < len(d2); i2++ {
		if d2[i2] != 0 {
			return -1
		}
	}
	return 0
}

func cmpAbs(a, b pgNumeric) int { return cmpAbsCommon(a.digits, a.weight, b.digits, b.weight) }

func cmpVar(a, b pgNumeric) int {
	if len(a.digits) == 0 {
		if len(b.digits) == 0 {
			return 0
		}
		if b.sign == numNeg {
			return 1
		}
		return -1
	}
	if len(b.digits) == 0 {
		if a.sign == numPos {
			return 1
		}
		return -1
	}
	if a.sign == numPos {
		if b.sign == numNeg {
			return 1
		}
		return cmpAbs(a, b)
	}
	if b.sign == numPos {
		return -1
	}
	return cmpAbs(b, a)
}

func addAbs(a, b pgNumeric) pgNumeric {
	resWeight := max(a.weight, b.weight) + 1
	resDscale := max(a.dscale, b.dscale)
	resRscale := max(len(a.digits)-a.weight-1, len(b.digits)-b.weight-1)
	resN := resRscale + resWeight + 1
	if resN <= 0 {
		resN = 1
	}
	res := make([]int32, resN)
	i1 := resRscale + a.weight + 1
	i2 := resRscale + b.weight + 1
	var carry int32
	for i := resN - 1; i >= 0; i-- {
		i1--
		i2--
		if i1 >= 0 && i1 < len(a.digits) {
			carry += a.digits[i1]
		}
		if i2 >= 0 && i2 < len(b.digits) {
			carry += b.digits[i2]
		}
		if carry >= nbase {
			res[i] = carry - nbase
			carry = 1
		} else {
			res[i] = carry
			carry = 0
		}
	}
	r := pgNumeric{digits: res, weight: resWeight, dscale: resDscale}
	r.strip()
	return r
}

// subAbs is |a| - |b|, requiring |a| >= |b|.
func subAbs(a, b pgNumeric) pgNumeric {
	resWeight := a.weight
	resDscale := max(a.dscale, b.dscale)
	resRscale := max(len(a.digits)-a.weight-1, len(b.digits)-b.weight-1)
	resN := resRscale + resWeight + 1
	if resN <= 0 {
		resN = 1
	}
	res := make([]int32, resN)
	i1 := resRscale + a.weight + 1
	i2 := resRscale + b.weight + 1
	var borrow int32
	for i := resN - 1; i >= 0; i-- {
		i1--
		i2--
		if i1 >= 0 && i1 < len(a.digits) {
			borrow += a.digits[i1]
		}
		if i2 >= 0 && i2 < len(b.digits) {
			borrow -= b.digits[i2]
		}
		if borrow < 0 {
			res[i] = borrow + nbase
			borrow = -1
		} else {
			res[i] = borrow
			borrow = 0
		}
	}
	r := pgNumeric{digits: res, weight: resWeight, dscale: resDscale}
	r.strip()
	return r
}

func addVar(a, b pgNumeric) pgNumeric {
	return addSigned(a, b, b.sign)
}

func subVar(a, b pgNumeric) pgNumeric {
	s := numNeg
	if b.sign == numNeg {
		s = numPos
	}
	return addSigned(a, b, s)
}

// addSigned is a + b where b's sign is taken as bsign.
func addSigned(a, b pgNumeric, bsign int) pgNumeric {
	zero := func() pgNumeric { return pgNumeric{dscale: max(a.dscale, b.dscale)} }
	if a.sign == numPos {
		if bsign == numPos {
			r := addAbs(a, b)
			r.sign = numPos
			return r
		}
		switch cmpAbs(a, b) {
		case 0:
			return zero()
		case 1:
			r := subAbs(a, b)
			r.sign = numPos
			if len(r.digits) == 0 {
				r.sign = numPos
			}
			return r
		}
		r := subAbs(b, a)
		r.sign = numNeg
		return r
	}
	if bsign == numPos {
		switch cmpAbs(a, b) {
		case 0:
			return zero()
		case 1:
			r := subAbs(a, b)
			r.sign = numNeg
			return r
		}
		r := subAbs(b, a)
		r.sign = numPos
		return r
	}
	r := addAbs(a, b)
	r.sign = numNeg
	return r
}

// mulVar is mul_var: a×b rounded to rscale, computed with guard digits only.
func mulVar(a, b pgNumeric, rscale int) pgNumeric {
	if len(a.digits) > len(b.digits) {
		a, b = b, a
	}
	n1, n2 := len(a.digits), len(b.digits)
	if n1 == 0 || n2 == 0 {
		return pgNumeric{dscale: rscale}
	}
	resSign := numNeg
	if a.sign == b.sign {
		resSign = numPos
	}
	resWeight := a.weight + b.weight + 2
	resN := n1 + n2 + 1
	maxdigits := resWeight + 1 + (rscale+decDigits-1)/decDigits + mulGuardDigits
	resN = min(resN, maxdigits)
	if resN < 3 {
		return pgNumeric{dscale: rscale}
	}
	dig := make([]int32, resN)
	var maxdig int32
	for i1 := min(n1-1, resN-3); i1 >= 0; i1-- {
		d1 := a.digits[i1]
		if d1 == 0 {
			continue
		}
		maxdig += d1
		if maxdig > (math.MaxInt32-math.MaxInt32/nbase)/(nbase-1) {
			var carry int32
			for i := resN - 1; i >= 0; i-- {
				newdig := dig[i] + carry
				if newdig >= nbase {
					carry = newdig / nbase
					newdig -= carry * nbase
				} else {
					carry = 0
				}
				dig[i] = newdig
			}
			maxdig = 1 + d1
		}
		lim := min(n2, resN-i1-2)
		for i2 := 0; i2 < lim; i2++ {
			dig[i1+2+i2] += d1 * b.digits[i2]
		}
	}
	res := make([]int32, resN)
	var carry int32
	for i := resN - 1; i >= 0; i-- {
		newdig := dig[i] + carry
		if newdig >= nbase {
			carry = newdig / nbase
			newdig -= carry * nbase
		} else {
			carry = 0
		}
		res[i] = newdig
	}
	r := pgNumeric{digits: res, weight: resWeight, sign: resSign}
	r.round(rscale)
	r.strip()
	return r
}

// divVar is div_var: a/b to exactly rscale digits, rounded or truncated (Knuth's algorithm D).
func divVar(a, b pgNumeric, rscale int, round bool) (pgNumeric, error) {
	n1, n2 := len(a.digits), len(b.digits)
	if n2 == 0 || b.digits[0] == 0 {
		return pgNumeric{}, errDivisionByZero
	}
	if n1 == 0 {
		return pgNumeric{dscale: rscale}, nil
	}
	resSign := numNeg
	if a.sign == b.sign {
		resSign = numPos
	}
	resWeight := a.weight - b.weight
	resN := resWeight + 1 + (rscale+decDigits-1)/decDigits
	resN = max(resN, 1)
	if round {
		resN++
	}
	divN := max(resN+n2, n1)
	dividend := make([]int32, divN+1)
	divisor := make([]int32, n2+1)
	copy(dividend[1:], a.digits)
	copy(divisor[1:], b.digits)
	res := make([]int32, resN)
	if n2 == 1 {
		divisor1 := divisor[1]
		var carry int32
		for i := 0; i < resN; i++ {
			carry = carry*nbase + dividend[i+1]
			res[i] = carry / divisor1
			carry = carry % divisor1
		}
	} else {
		if divisor[1] < halfNbase {
			d := nbase / (divisor[1] + 1)
			var carry int32
			for i := n2; i > 0; i-- {
				carry += divisor[i] * d
				divisor[i] = carry % nbase
				carry = carry / nbase
			}
			carry = 0
			for i := n1; i >= 0; i-- {
				carry += dividend[i] * d
				dividend[i] = carry % nbase
				carry = carry / nbase
			}
		}
		divisor1, divisor2 := divisor[1], divisor[2]
		for j := 0; j < resN; j++ {
			next2 := dividend[j]*nbase + dividend[j+1]
			if next2 == 0 {
				res[j] = 0
				continue
			}
			var qhat int32
			if dividend[j] == divisor1 {
				qhat = nbase - 1
			} else {
				qhat = next2 / divisor1
			}
			for divisor2*qhat > (next2-qhat*divisor1)*nbase+dividend[j+2] {
				qhat--
			}
			if qhat > 0 {
				var carry, borrow int32
				for i := n2; i >= 0; i-- {
					carry += divisor[i] * qhat
					borrow -= carry % nbase
					carry = carry / nbase
					borrow += dividend[j+i]
					if borrow < 0 {
						dividend[j+i] = borrow + nbase
						borrow = -1
					} else {
						dividend[j+i] = borrow
						borrow = 0
					}
				}
				if borrow != 0 {
					qhat--
					carry = 0
					for i := n2; i >= 0; i-- {
						carry += dividend[j+i] + divisor[i]
						if carry >= nbase {
							dividend[j+i] = carry - nbase
							carry = 1
						} else {
							dividend[j+i] = carry
							carry = 0
						}
					}
				}
			}
			res[j] = qhat
		}
	}
	r := pgNumeric{digits: res, weight: resWeight, sign: resSign}
	if round {
		r.round(rscale)
	} else {
		r.trunc(rscale)
	}
	r.strip()
	return r, nil
}

// divVarFast is div_var_fast: the FM library's division, quotient digits estimated in floating
// point, as Postgres uses in its transcendental functions.
func divVarFast(a, b pgNumeric, rscale int, round bool) (pgNumeric, error) {
	n1, n2 := len(a.digits), len(b.digits)
	if n2 == 0 || b.digits[0] == 0 {
		return pgNumeric{}, errDivisionByZero
	}
	if n1 == 0 {
		return pgNumeric{dscale: rscale}, nil
	}
	resSign := numNeg
	if a.sign == b.sign {
		resSign = numPos
	}
	resWeight := a.weight - b.weight + 1
	divN := resWeight + 1 + (rscale+decDigits-1)/decDigits
	divN += divGuardDigits
	if divN < divGuardDigits {
		divN = divGuardDigits
	}
	div := make([]int32, divN+1)
	load := min(divN, n1)
	for i := 0; i < load; i++ {
		div[i+1] = a.digits[i]
	}
	fdivisor := float64(b.digits[0])
	for i := 1; i < 4; i++ {
		fdivisor = float64(fdivisor * nbase)
		if i < n2 {
			fdivisor += float64(b.digits[i])
		}
	}
	fdivisorinverse := 1.0 / fdivisor
	estimate := func(qi int) int32 {
		fdividend := float64(div[qi])
		for i := 1; i < 4; i++ {
			fdividend = float64(fdividend * nbase)
			if qi+i <= divN {
				fdividend += float64(div[qi+i])
			}
		}
		fq := float64(fdividend * fdivisorinverse)
		if fq >= 0.0 {
			return int32(fq)
		}
		return int32(fq) - 1
	}
	maxdiv := int32(1)
	qi := 0
	for ; qi < divN; qi++ {
		qdigit := estimate(qi)
		if qdigit != 0 {
			maxdiv += abs32(qdigit)
			if maxdiv > (math.MaxInt32-math.MaxInt32/nbase-1)/(nbase-1) {
				var carry int32
				for i := min(qi+n2-2, divN); i > qi; i-- {
					newdig := div[i] + carry
					if newdig < 0 {
						carry = -((-newdig - 1) / nbase) - 1
						newdig -= carry * nbase
					} else if newdig >= nbase {
						carry = newdig / nbase
						newdig -= carry * nbase
					} else {
						carry = 0
					}
					div[i] = newdig
				}
				div[qi] += carry
				maxdiv = 1
				qdigit = estimate(qi)
				maxdiv += abs32(qdigit)
			}
			if qdigit != 0 {
				istop := min(n2, divN-qi+1)
				for i := 0; i < istop; i++ {
					div[qi+i] -= qdigit * b.digits[i]
				}
			}
		}
		div[qi+1] += div[qi] * nbase
		div[qi] = qdigit
	}
	{
		fdividend := float64(div[qi])
		for i := 1; i < 4; i++ {
			fdividend = float64(fdividend * nbase)
		}
		fq := float64(fdividend * fdivisorinverse)
		if fq >= 0.0 {
			div[qi] = int32(fq)
		} else {
			div[qi] = int32(fq) - 1
		}
	}
	res := make([]int32, divN+1)
	var carry int32
	for i := divN; i >= 0; i-- {
		newdig := div[i] + carry
		if newdig < 0 {
			carry = -((-newdig - 1) / nbase) - 1
			newdig -= carry * nbase
		} else if newdig >= nbase {
			carry = newdig / nbase
			newdig -= carry * nbase
		} else {
			carry = 0
		}
		res[i] = newdig
	}
	r := pgNumeric{digits: res, weight: resWeight, sign: resSign}
	if round {
		r.round(rscale)
	} else {
		r.trunc(rscale)
	}
	r.strip()
	return r, nil
}

func abs32(x int32) int32 {
	if x < 0 {
		return -x
	}
	return x
}

// selectDivScale is select_div_scale: enough digits for 16 significant, and no less than either
// input's display scale.
func selectDivScale(a, b pgNumeric) int {
	weight1, first1 := 0, int32(0)
	for i, d := range a.digits {
		first1 = d
		if d != 0 {
			weight1 = a.weight - i
			break
		}
	}
	weight2, first2 := 0, int32(0)
	for i, d := range b.digits {
		first2 = d
		if d != 0 {
			weight2 = b.weight - i
			break
		}
	}
	qweight := weight1 - weight2
	if first1 <= first2 {
		qweight--
	}
	rscale := numericMinSigDigit - qweight*decDigits
	rscale = max(rscale, a.dscale, b.dscale, numericMinDisplay)
	return min(rscale, numericMaxDisplay)
}

func modVar(a, b pgNumeric) (pgNumeric, error) {
	q, err := divVar(a, b, 0, false)
	if err != nil {
		return pgNumeric{}, err
	}
	return subVar(a, mulVar(b, q, b.dscale)), nil
}

func divModVar(a, b pgNumeric) (pgNumeric, pgNumeric, error) {
	q, err := divVarFast(a, b, 0, false)
	if err != nil {
		return pgNumeric{}, pgNumeric{}, err
	}
	r := subVar(a, mulVar(b, q, b.dscale))
	for len(r.digits) != 0 && r.sign != a.sign {
		if a.sign == b.sign {
			q = subVar(q, numOne)
			r = addVar(r, b)
		} else {
			q = addVar(q, numOne)
			r = subVar(r, b)
		}
	}
	for cmpAbs(r, b) >= 0 {
		if a.sign == b.sign {
			q = addVar(q, numOne)
			r = subVar(r, b)
		} else {
			q = subVar(q, numOne)
			r = addVar(r, b)
		}
	}
	return q, r, nil
}

func ceilVar(v pgNumeric) pgNumeric {
	t := v.clone()
	t.trunc(0)
	if v.sign == numPos && cmpVar(v, t) != 0 {
		t = addVar(t, numOne)
	}
	return t
}

func floorVar(v pgNumeric) pgNumeric {
	t := v.clone()
	t.trunc(0)
	if v.sign == numNeg && cmpVar(v, t) != 0 {
		t = subVar(t, numOne)
	}
	return t
}

// round is round_var: to rscale decimal digits after the point, rscale < 0 rounding before it.
func (n *pgNumeric) round(rscale int) {
	n.dscale = rscale
	di := (n.weight+1)*decDigits + rscale
	if di < 0 {
		n.digits = nil
		n.weight = 0
		n.sign = numPos
		return
	}
	nd := (di + decDigits - 1) / decDigits
	di %= decDigits
	if !(nd < len(n.digits) || (nd == len(n.digits) && di > 0)) {
		return
	}
	digits := append([]int32{0}, n.digits...) // a spare leading digit for the carry
	keep := nd
	var carry int32
	if di == 0 {
		if digits[1+nd] >= halfNbase {
			carry = 1
		}
	} else {
		pow10 := roundPowers[di]
		nd--
		extra := digits[1+nd] % pow10
		digits[1+nd] -= extra
		if extra >= pow10/2 {
			pow10 += digits[1+nd]
			if pow10 >= nbase {
				pow10 -= nbase
				carry = 1
			}
			digits[1+nd] = pow10
		}
	}
	for carry != 0 {
		nd--
		carry += digits[1+nd]
		if carry >= nbase {
			digits[1+nd] = carry - nbase
			carry = 1
		} else {
			digits[1+nd] = carry
			carry = 0
		}
	}
	if nd < 0 {
		n.digits = digits[:keep+1]
		n.weight++
	} else {
		n.digits = digits[1 : 1+keep]
	}
}

// trunc is trunc_var: toward zero at rscale decimal digits.
func (n *pgNumeric) trunc(rscale int) {
	n.dscale = rscale
	di := (n.weight+1)*decDigits + rscale
	if di <= 0 {
		n.digits = nil
		n.weight = 0
		n.sign = numPos
		return
	}
	nd := (di + decDigits - 1) / decDigits
	if nd <= len(n.digits) {
		d := append([]int32(nil), n.digits[:nd]...)
		di %= decDigits
		if di > 0 {
			pow10 := roundPowers[di]
			d[nd-1] -= d[nd-1] % pow10
		}
		n.digits = d
	}
}

func int64ToNumeric(v int64) pgNumeric {
	if v == 0 {
		return pgNumeric{}
	}
	r := pgNumeric{}
	var u uint64
	if v < 0 {
		r.sign = numNeg
		u = uint64(-v)
	} else {
		u = uint64(v)
	}
	var d []int32
	for u != 0 {
		d = append([]int32{int32(u % nbase)}, d...)
		u /= nbase
	}
	r.digits = d
	r.weight = len(d) - 1
	return r
}

// toInt64 is numericvar_to_int64: rounded to the nearest integer; false on overflow.
func (n pgNumeric) toInt64() (int64, bool) {
	r := n.clone()
	r.round(0)
	r.strip()
	if len(r.digits) == 0 {
		return 0, true
	}
	val := -int64(r.digits[0])
	for i := 1; i <= r.weight; i++ {
		if val < math.MinInt64/nbase {
			return 0, false
		}
		val *= nbase
		if i < len(r.digits) {
			if val < math.MinInt64+int64(r.digits[i]) {
				return 0, false
			}
			val -= int64(r.digits[i])
		}
	}
	if r.sign != numNeg {
		if val == math.MinInt64 {
			return 0, false
		}
		val = -val
	}
	return val, true
}

// toFloat64 is numericvar_to_double_no_overflow: the text read as a double.
func (n pgNumeric) toFloat64() float64 {
	switch n.sign {
	case numNaN:
		return math.NaN()
	case numPInf:
		return math.Inf(1)
	case numNInf:
		return math.Inf(-1)
	}
	f, _ := strconv.ParseFloat(n.String(), 64)
	return f
}

// float8ToNumeric is float8_numeric: the value printed with 15 significant digits.
func float8ToNumeric(f float64) (pgNumeric, error) {
	switch {
	case math.IsNaN(f):
		return numNaNVal, nil
	case math.IsInf(f, 1):
		return numPInfVal, nil
	case math.IsInf(f, -1):
		return numNInfVal, nil
	}
	s := strconv.FormatFloat(f, 'g', 15, 64)
	v, _, err := setVarFromStr(s, 0)
	if err != nil {
		return pgNumeric{}, err
	}
	return makeResult(v)
}

// pgNumericOf is a value as numeric: an integer exactly, a provider's number by the shortest text
// that reads back as it (the decimal it was written as), and text by numeric_in.
func pgNumericOf(v any) (pgNumeric, error) {
	switch x := v.(type) {
	case pgNumeric:
		return x, nil
	case int64:
		return int64ToNumeric(x), nil
	case int:
		return int64ToNumeric(int64(x)), nil
	case pgInt4:
		return int64ToNumeric(int64(x)), nil
	case int32:
		return int64ToNumeric(int64(x)), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return float8ToNumeric(x)
		}
		return parseNumeric(strconv.FormatFloat(x, 'f', -1, 64))
	case float32:
		return pgNumericOf(float64(x))
	case json.Number:
		return parseNumeric(string(x))
	case pgFloat8:
		return float8ToNumeric(float64(x))
	case string:
		return parseNumeric(x)
	case pgUnknown:
		return parseNumeric(string(x))
	}
	return pgNumeric{}, errors.New("not a number")
}

// sqrtVar is sqrt_var: the exact integer square root of the digits that bear on rscale, rounded.
func sqrtVar(arg pgNumeric, rscale int) (pgNumeric, error) {
	stat := cmpVar(arg, numZero)
	if stat == 0 {
		return pgNumeric{dscale: rscale}, nil
	}
	if stat < 0 {
		return pgNumeric{}, pgErrorf("cannot take square root of a negative number")
	}
	var resWeight int
	if arg.weight >= 0 {
		resWeight = arg.weight / 2
	} else {
		resWeight = -((-arg.weight-1)/2 + 1)
	}
	var resN int
	if rscale+1 >= 0 {
		resN = resWeight + 1 + (rscale+decDigits)/decDigits
	} else {
		resN = resWeight + 1 - (-rscale-1)/decDigits
	}
	resN = max(resN, 1)
	srcN := arg.weight + 1 + (resN-resWeight-1)*2
	srcN = max(srcN, 1)
	// The Karatsuba algorithm Postgres runs yields the exact floor square root of the first srcN
	// input digits, so that is computed directly.
	n := new(big.Int)
	base := big.NewInt(nbase)
	for i := 0; i < srcN; i++ {
		d := int64(0)
		if i < len(arg.digits) {
			d = int64(arg.digits[i])
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(d))
	}
	s := new(big.Int).Sqrt(n)
	var digits []int32
	q, m := new(big.Int), new(big.Int)
	for s.Sign() > 0 {
		q.DivMod(s, base, m)
		digits = append([]int32{int32(m.Int64())}, digits...)
		s.Set(q)
	}
	// As Postgres does, the root's first digit takes the result weight.
	res := pgNumeric{digits: digits, weight: resWeight}
	res.round(rscale)
	res.strip()
	return res, nil
}

// expVar is exp_var.
func expVar(arg pgNumeric, rscale int) (pgNumeric, error) {
	x := arg.clone()
	val := x.toFloat64()
	if math.Abs(val) >= numericMaxResult*3 {
		if val > 0 {
			return pgNumeric{}, errNumericOverflow
		}
		return pgNumeric{dscale: rscale}, nil
	}
	dweight := int(float64(val * 0.434294481903252))
	ndiv2 := 0
	var err error
	if math.Abs(val) > 0.01 {
		tmp := numTwo.clone()
		ndiv2 = 1
		val /= 2
		for math.Abs(val) > 0.01 {
			ndiv2++
			val /= 2
			tmp = addVar(tmp, tmp)
		}
		x, err = divVarFast(x, tmp, x.dscale+ndiv2, true)
		if err != nil {
			return pgNumeric{}, err
		}
	}
	sigDigits := 1 + dweight + rscale + int(float64(float64(ndiv2)*0.301029995663981))
	sigDigits = max(sigDigits, 0) + 8
	localRscale := sigDigits - 1
	result := addVar(numOne, x)
	elem := mulVar(x, x, localRscale)
	ni := numTwo.clone()
	elem, err = divVarFast(elem, ni, localRscale, true)
	if err != nil {
		return pgNumeric{}, err
	}
	for len(elem.digits) != 0 {
		result = addVar(result, elem)
		elem = mulVar(elem, x, localRscale)
		ni = addVar(ni, numOne)
		elem, err = divVarFast(elem, ni, localRscale, true)
		if err != nil {
			return pgNumeric{}, err
		}
	}
	for ; ndiv2 > 0; ndiv2-- {
		lr := sigDigits - result.weight*2*decDigits
		lr = max(lr, numericMinDisplay)
		result = mulVar(result, result, lr)
	}
	result.round(rscale)
	return result, nil
}

// estimateLnDweight is estimate_ln_dweight.
func estimateLnDweight(v pgNumeric) int {
	if v.sign != numPos {
		return 0
	}
	if cmpVar(v, numPoint9) >= 0 && cmpVar(v, numOnePt1) <= 0 {
		x := subVar(v, numOne)
		if len(x.digits) > 0 {
			return x.weight*decDigits + int(math.Log10(float64(x.digits[0])))
		}
		return 0
	}
	if len(v.digits) == 0 {
		return 0
	}
	digits := v.digits[0]
	dweight := v.weight * decDigits
	if len(v.digits) > 1 {
		digits = digits*nbase + v.digits[1]
		dweight -= decDigits
	}
	lnv := math.Log(float64(digits)) + float64(float64(dweight)*2.302585092994046)
	return int(math.Log10(math.Abs(lnv)))
}

// lnVar is ln_var.
func lnVar(arg pgNumeric, rscale int) (pgNumeric, error) {
	switch c := cmpVar(arg, numZero); {
	case c == 0:
		return pgNumeric{}, pgErrorf("cannot take logarithm of zero")
	case c < 0:
		return pgNumeric{}, pgErrorf("cannot take logarithm of a negative number")
	}
	x := arg.clone()
	fact := numTwo.clone()
	nsqrt := 0
	var err error
	for cmpVar(x, numPoint9) <= 0 {
		lr := rscale - x.weight*decDigits/2 + 8
		if x, err = sqrtVar(x, lr); err != nil {
			return pgNumeric{}, err
		}
		fact = mulVar(fact, numTwo, 0)
		nsqrt++
	}
	for cmpVar(x, numOnePt1) >= 0 {
		lr := rscale - x.weight*decDigits/2 + 8
		if x, err = sqrtVar(x, lr); err != nil {
			return pgNumeric{}, err
		}
		fact = mulVar(fact, numTwo, 0)
		nsqrt++
	}
	lr := rscale + int(float64(float64(nsqrt+1)*0.301029995663981)) + 8
	result := subVar(x, numOne)
	elem := addVar(x, numOne)
	if result, err = divVarFast(result, elem, lr, true); err != nil {
		return pgNumeric{}, err
	}
	xx := result.clone()
	x = mulVar(result, result, lr)
	ni := numOne.clone()
	for {
		ni = addVar(ni, numTwo)
		xx = mulVar(xx, x, lr)
		if elem, err = divVarFast(xx, ni, lr, true); err != nil {
			return pgNumeric{}, err
		}
		if len(elem.digits) == 0 {
			break
		}
		result = addVar(result, elem)
		if elem.weight < result.weight-lr*2/decDigits {
			break
		}
	}
	return mulVar(result, fact, rscale), nil
}

// logVar is log_var: log of num in base, choosing the result's scale.
func logVar(base, num pgNumeric) (pgNumeric, error) {
	lbd := estimateLnDweight(base)
	lnd := estimateLnDweight(num)
	rd := lnd - lbd
	rscale := numericMinSigDigit - rd
	rscale = max(rscale, base.dscale, num.dscale, numericMinDisplay)
	rscale = min(rscale, numericMaxDisplay)
	lbr := max(rscale+rd-lbd+8, numericMinDisplay)
	lnr := max(rscale+rd-lnd+8, numericMinDisplay)
	lnBase, err := lnVar(base, lbr)
	if err != nil {
		return pgNumeric{}, err
	}
	lnNum, err := lnVar(num, lnr)
	if err != nil {
		return pgNumeric{}, err
	}
	return divVarFast(lnNum, lnBase, rscale, true)
}

// powerVar is power_var.
func powerVar(base, exp pgNumeric) (pgNumeric, error) {
	if len(exp.digits) == 0 || len(exp.digits) <= exp.weight+1 {
		if e64, ok := exp.toInt64(); ok && e64 >= math.MinInt32 && e64 <= math.MaxInt32 {
			rscale := max(numericMinSigDigit, base.dscale, numericMinDisplay)
			rscale = min(rscale, numericMaxDisplay)
			return powerVarInt(base, int(e64), rscale)
		}
	}
	if cmpVar(base, numZero) == 0 {
		return pgNumeric{dscale: numericMinSigDigit}, nil
	}
	resSign := numPos
	if base.sign == numNeg {
		if len(exp.digits) > 0 && len(exp.digits) > exp.weight+1 {
			return pgNumeric{}, pgErrorf("a negative number raised to a non-integer power yields a complex result")
		}
		if len(exp.digits) > 0 && len(exp.digits) == exp.weight+1 && exp.digits[len(exp.digits)-1]&1 != 0 {
			resSign = numNeg
		}
		base = base.clone()
		base.sign = numPos
	}
	lnDweight := estimateLnDweight(base)
	lr := max(8-lnDweight, numericMinDisplay)
	lnBase, err := lnVar(base, lr)
	if err != nil {
		return pgNumeric{}, err
	}
	lnNum := mulVar(lnBase, exp, lr)
	val := lnNum.toFloat64()
	if math.Abs(val) > numericMaxResult*3.01 {
		if val > 0 {
			return pgNumeric{}, errNumericOverflow
		}
		return pgNumeric{dscale: numericMaxDisplay}, nil
	}
	val = float64(val * 0.434294481903252)
	rscale := numericMinSigDigit - int(val)
	rscale = max(rscale, base.dscale, exp.dscale, numericMinDisplay)
	rscale = min(rscale, numericMaxDisplay)
	sig := max(rscale+int(val), 0)
	lr = max(sig-lnDweight+8, numericMinDisplay)
	if lnBase, err = lnVar(base, lr); err != nil {
		return pgNumeric{}, err
	}
	lnNum = mulVar(lnBase, exp, lr)
	result, err := expVar(lnNum, rscale)
	if err != nil {
		return pgNumeric{}, err
	}
	if resSign == numNeg && len(result.digits) > 0 {
		result.sign = numNeg
	}
	return result, nil
}

// powerVarInt is power_var_int.
func powerVarInt(base pgNumeric, exp int, rscale int) (pgNumeric, error) {
	switch exp {
	case 0:
		return pgNumeric{digits: []int32{1}, dscale: rscale}, nil
	case 1:
		r := base.clone()
		r.round(rscale)
		return r, nil
	case -1:
		return divVar(numOne, base, rscale, true)
	case 2:
		return mulVar(base, base, rscale), nil
	}
	if len(base.digits) == 0 {
		if exp < 0 {
			return pgNumeric{}, errDivisionByZero
		}
		return pgNumeric{dscale: rscale}, nil
	}
	f := float64(base.digits[0])
	p := base.weight * decDigits
	for i := 1; i < len(base.digits) && i*decDigits < 16; i++ {
		f = float64(f*nbase) + float64(base.digits[i])
		p -= decDigits
	}
	f = float64(float64(exp) * (math.Log10(f) + float64(p)))
	if f > 3*math.MaxInt16*decDigits {
		return pgNumeric{}, errNumericOverflow
	}
	if f+1 < float64(-rscale) || f+1 < -numericMaxDisplay {
		return pgNumeric{dscale: rscale}, nil
	}
	sig := 1 + rscale + int(f)
	sig += int(math.Log(math.Abs(float64(exp)))) + 8
	neg := exp < 0
	mask := uint32(exp)
	if neg {
		mask = uint32(-exp)
	}
	baseProd := base.clone()
	var result pgNumeric
	if mask&1 != 0 {
		result = base.clone()
	} else {
		result = numOne.clone()
	}
	for mask >>= 1; mask > 0; mask >>= 1 {
		lr := sig - 2*baseProd.weight*decDigits
		lr = min(lr, 2*baseProd.dscale)
		lr = max(lr, numericMinDisplay)
		baseProd = mulVar(baseProd, baseProd, lr)
		if mask&1 != 0 {
			lr = sig - (baseProd.weight+result.weight)*decDigits
			lr = min(lr, baseProd.dscale+result.dscale)
			lr = max(lr, numericMinDisplay)
			result = mulVar(baseProd, result, lr)
		}
		if baseProd.weight > math.MaxInt16 || result.weight > math.MaxInt16 {
			if !neg {
				return pgNumeric{}, errNumericOverflow
			}
			result = pgNumeric{dscale: result.dscale}
			neg = false
			break
		}
	}
	if neg {
		return divVarFast(numOne, result, rscale, true)
	}
	result.round(rscale)
	return result, nil
}

// getMinScale is get_min_scale: the fewest decimal places that show the value exactly.
func (n pgNumeric) minScale() int {
	last := len(n.digits) - 1
	for last >= 0 && n.digits[last] == 0 {
		last--
	}
	if last < 0 {
		return 0
	}
	ms := (last - n.weight) * decDigits
	if ms <= 0 {
		return 0
	}
	d := n.digits[last]
	for d%10 == 0 {
		ms--
		d /= 10
	}
	return ms
}

func (n pgNumeric) signum() int {
	switch {
	case n.sign == numPInf:
		return 1
	case n.sign == numNInf:
		return -1
	case len(n.digits) == 0:
		return 0
	case n.sign == numNeg:
		return -1
	}
	return 1
}

// isIntegral is numeric_is_integral.
func (n pgNumeric) isIntegral() bool {
	if n.isSpecial() {
		return n.isInf()
	}
	return len(n.digits) == 0 || len(n.digits) <= n.weight+1
}
