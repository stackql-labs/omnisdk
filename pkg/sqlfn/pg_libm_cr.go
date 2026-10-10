package sqlfn

import (
	"math"
	"math/big"
	"sync"
)

// Correctly rounded trigonometry, for the Postgres catalogue. stackql's Postgres image links glibc
// 2.31, whose tan, atan, atan2, asin and acos are correctly rounded (they fall back to multiple
// precision where the fast path is unsure) and whose sin and cos are within 0.55 ulp, which is
// correctly rounded but for vanishingly rare arguments. A correctly rounded result is unique, so it is
// computed here to the nearest double from a 320-bit evaluation: the same result glibc returns.

const crPrec = 320

var (
	crPiOnce sync.Once
	crPi     *big.Float // to 2400 bits: enough to reduce any double argument
)

func bigF(prec uint) *big.Float { return new(big.Float).SetPrec(prec) }

func crPiValue() *big.Float {
	crPiOnce.Do(func() {
		const p = 2400
		// Machin: pi = 16 atan(1/5) - 4 atan(1/239).
		a := atanSeriesRecip(5, p)
		b := atanSeriesRecip(239, p)
		a.Mul(a, bigF(p).SetInt64(16))
		b.Mul(b, bigF(p).SetInt64(4))
		crPi = a.Sub(a, b)
	})
	return crPi
}

// atanSeriesRecip is atan(1/n) by its Taylor series.
func atanSeriesRecip(n int64, prec uint) *big.Float {
	x := bigF(prec).Quo(bigF(prec).SetInt64(1), bigF(prec).SetInt64(n))
	x2 := bigF(prec).Mul(x, x)
	sum := bigF(prec).Set(x)
	term := bigF(prec).Set(x)
	eps := bigF(prec).SetMantExp(bigF(prec).SetInt64(1), -int(prec)-8)
	for k := int64(1); ; k++ {
		term.Mul(term, x2)
		t := bigF(prec).Quo(term, bigF(prec).SetInt64(2*k+1))
		if k%2 == 1 {
			sum.Sub(sum, t)
		} else {
			sum.Add(sum, t)
		}
		if t.Abs(t).Cmp(eps) < 0 {
			return sum
		}
	}
}

// crSinCos is sin and cos of x by reduction modulo pi/2 at high precision, then their series.
func crSinCos(x float64) (sin, cos *big.Float) {
	const p = 2400
	pi := crPiValue()
	halfPi := bigF(p).Quo(pi, bigF(p).SetInt64(2))
	bx := bigF(p).SetFloat64(x)
	q := bigF(p).Quo(bx, halfPi)
	// k = round(q), as an integer.
	kInt, _ := q.Int(nil)
	frac := bigF(p).Sub(q, bigF(p).SetInt(kInt))
	half := bigF(p).SetFloat64(0.5)
	if frac.Cmp(half) > 0 {
		kInt.Add(kInt, big.NewInt(1))
	} else if frac.Cmp(bigF(p).Neg(half)) < 0 {
		kInt.Sub(kInt, big.NewInt(1))
	}
	r := bigF(p).Sub(bx, bigF(p).Mul(bigF(p).SetInt(kInt), halfPi))
	r = bigF(crPrec).Set(r)
	s, c := sinCosSeries(r)
	switch new(big.Int).And(kInt, big.NewInt(3)).Int64() {
	case 0:
		return s, c
	case 1:
		return c, s.Neg(s)
	case 2:
		return s.Neg(s), c.Neg(c)
	}
	return c.Neg(c), s
}

func sinCosSeries(r *big.Float) (*big.Float, *big.Float) {
	const p = crPrec
	r2 := bigF(p).Mul(r, r)
	eps := bigF(p).SetMantExp(bigF(p).SetInt64(1), -p-8)
	sin := bigF(p).Set(r)
	term := bigF(p).Set(r)
	for k := int64(1); ; k++ {
		term.Mul(term, r2)
		term.Quo(term, bigF(p).SetInt64((2*k)*(2*k+1)))
		term.Neg(term)
		sin.Add(sin, term)
		if bigF(p).Abs(term).Cmp(eps) < 0 {
			break
		}
	}
	cos := bigF(p).SetInt64(1)
	term = bigF(p).SetInt64(1)
	for k := int64(1); ; k++ {
		term.Mul(term, r2)
		term.Quo(term, bigF(p).SetInt64((2*k-1)*(2*k)))
		term.Neg(term)
		cos.Add(cos, term)
		if bigF(p).Abs(term).Cmp(eps) < 0 {
			break
		}
	}
	return sin, cos
}

// crAtanBig is atan(x) at high precision: halved by atan(x) = 2 atan(x/(1+sqrt(1+x²))) until small,
// then its series.
func crAtanBig(x *big.Float) *big.Float {
	const p = crPrec + 64
	v := bigF(p).Set(x)
	halvings := 0
	small := bigF(p).SetFloat64(1.0 / 64)
	one := bigF(p).SetInt64(1)
	for bigF(p).Abs(v).Cmp(small) > 0 {
		s := bigF(p).Mul(v, v)
		s.Add(s, one)
		s.Sqrt(s)
		s.Add(s, one)
		v.Quo(v, s)
		halvings++
	}
	v2 := bigF(p).Mul(v, v)
	sum := bigF(p).Set(v)
	term := bigF(p).Set(v)
	eps := bigF(p).SetMantExp(one, -p-8)
	for k := int64(1); ; k++ {
		term.Mul(term, v2)
		t := bigF(p).Quo(term, bigF(p).SetInt64(2*k+1))
		if k%2 == 1 {
			sum.Sub(sum, t)
		} else {
			sum.Add(sum, t)
		}
		if t.Abs(t).Cmp(eps) < 0 {
			break
		}
	}
	return sum.SetMantExp(sum, halvings)
}

func crRound(v *big.Float) float64 {
	f, _ := v.Float64()
	return f
}

func (glibcLibm) Tan(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return math.NaN()
	}
	if x == 0 {
		return x
	}
	s, c := crSinCos(x)
	return crRound(s.Quo(s, c))
}

func (glibcLibm) Atan(x float64) float64 {
	if math.IsNaN(x) || x == 0 {
		return x
	}
	if math.IsInf(x, 0) {
		return math.Copysign(math.Pi/2, x)
	}
	return crRound(crAtanBig(bigF(crPrec).SetFloat64(x)))
}

func (glibcLibm) Asin(x float64) float64 {
	if math.IsNaN(x) || x == 0 {
		return x
	}
	if x < -1 || x > 1 {
		return math.NaN()
	}
	const p = crPrec + 64
	bx := bigF(p).SetFloat64(x)
	if x == 1 || x == -1 {
		h := bigF(p).Quo(crPiValue(), bigF(p).SetInt64(2))
		if x < 0 {
			h.Neg(h)
		}
		return crRound(h)
	}
	// asin x = atan(x / sqrt(1 - x²)).
	d := bigF(p).Mul(bx, bx)
	d.Sub(bigF(p).SetInt64(1), d)
	d.Sqrt(d)
	return crRound(crAtanBig(bigF(p).Quo(bx, d)))
}

func (glibcLibm) Acos(x float64) float64 {
	if math.IsNaN(x) {
		return x
	}
	if x < -1 || x > 1 {
		return math.NaN()
	}
	const p = crPrec + 64
	if x == 1 {
		return 0
	}
	if x == -1 {
		return crRound(bigF(p).Set(crPiValue()))
	}
	// acos x = 2 atan(sqrt((1 - x)/(1 + x))).
	bx := bigF(p).SetFloat64(x)
	one := bigF(p).SetInt64(1)
	n := bigF(p).Sub(one, bx)
	d := bigF(p).Add(one, bx)
	n.Quo(n, d)
	n.Sqrt(n)
	a := crAtanBig(n)
	return crRound(a.Mul(a, bigF(p).SetInt64(2)))
}

func (l glibcLibm) Atan2(y, x float64) float64 {
	if math.IsNaN(x) || math.IsNaN(y) {
		return math.NaN()
	}
	// The special cases, as C99 F.9.1.4 and glibc have them.
	if y == 0 || math.IsInf(x, 0) || math.IsInf(y, 0) || x == 0 {
		return math.Atan2(y, x)
	}
	const p = crPrec + 64
	pi := bigF(p).Set(crPiValue())
	a := crAtanBig(bigF(p).Quo(bigF(p).SetFloat64(math.Abs(y)), bigF(p).SetFloat64(math.Abs(x))))
	if x < 0 {
		a = bigF(p).Sub(pi, a)
	}
	if y < 0 {
		a.Neg(a)
	}
	return crRound(a)
}
