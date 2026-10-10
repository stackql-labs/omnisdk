package sqlfn

import (
	"math"
)

// Postgres's math functions: float8 ones as float.c has them, over the C library stackql's Postgres
// runs on (pgLibm), and numeric ones over the numeric.c port.

var (
	errFloatOverflow  = pgErrorf("value out of range: overflow")
	errFloatUnderflow = pgErrorf("value out of range: underflow")
	errOutOfRange     = pgErrorf("input is out of range")
	errIntOutOfRange  = pgErrorf("integer out of range")
	errInt8OutOfRange = pgErrorf("bigint out of range")
	errLogZero        = pgErrorf("cannot take logarithm of zero")
	errLogNegative    = pgErrorf("cannot take logarithm of a negative number")
	errSqrtNegative   = pgErrorf("cannot take square root of a negative number")
)

const radiansPerDegree = 0.0174532925199432957692

func f8(a any) float64    { return float64(a.(pgFloat8)) }
func num(a any) pgNumeric { return a.(pgNumeric) }
func i4(a any) int32      { return int32(a.(pgInt4)) }
func i8(a any) int64      { return a.(int64) }
func f8r(f float64) any   { return pgFloat8(f) }
func numr(n pgNumeric) (any, error) {
	r, err := makeResult(n)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func init() {
	unaryF8 := func(name string, f func(float64) (float64, error)) {
		registerPg(name+"(float8)", func(a []any) (any, error) {
			r, err := f(f8(a[0]))
			if err != nil {
				return nil, err
			}
			return f8r(r), nil
		})
	}
	unaryF8("abs", func(x float64) (float64, error) { return math.Abs(x), nil })
	for _, n := range []string{"ceil", "ceiling"} {
		unaryF8(n, func(x float64) (float64, error) { return math.Ceil(x), nil })
	}
	unaryF8("floor", func(x float64) (float64, error) { return math.Floor(x), nil })
	unaryF8("round", func(x float64) (float64, error) { return math.RoundToEven(x), nil })
	unaryF8("trunc", func(x float64) (float64, error) {
		if x >= 0 {
			return math.Floor(x), nil
		}
		return -math.Floor(-x), nil
	})
	unaryF8("sign", func(x float64) (float64, error) {
		switch {
		case x > 0:
			return 1, nil
		case x < 0:
			return -1, nil
		}
		return 0, nil
	})
	unaryF8("sqrt", func(x float64) (float64, error) {
		if x < 0 {
			return 0, errSqrtNegative
		}
		r := math.Sqrt(x)
		return checkFloat(r, math.IsInf(x, 0), x == 0)
	})
	// The functions glibc builds differently on each architecture read the catalogue's libm.
	unaryEnv := func(name string, f func(l pgLibmFuncs, x float64) (float64, error)) {
		registerPgEnv(name+"(float8)", func(e pgEnv, a []any) (any, error) {
			r, err := f(e.libm, f8(a[0]))
			if err != nil {
				return nil, err
			}
			return f8r(r), nil
		})
	}
	unaryEnv("cbrt", func(l pgLibmFuncs, x float64) (float64, error) {
		r := l.Cbrt(x)
		return checkFloat(r, math.IsInf(x, 0), x == 0)
	})
	unaryF8("exp", func(x float64) (float64, error) {
		switch {
		case math.IsNaN(x):
			return x, nil
		case math.IsInf(x, 0):
			if x > 0 {
				return x, nil
			}
			return 0, nil
		}
		r := pgLibm.Exp(x)
		if math.IsInf(r, 0) {
			return 0, errFloatOverflow
		}
		if r == 0 {
			return 0, errFloatUnderflow
		}
		return r, nil
	})
	logf := func(f func(float64) float64) func(float64) (float64, error) {
		return func(x float64) (float64, error) {
			if x == 0 {
				return 0, errLogZero
			}
			if x < 0 {
				return 0, errLogNegative
			}
			r := f(x)
			if math.IsInf(r, 0) && !math.IsInf(x, 0) {
				return 0, errFloatOverflow
			}
			if r == 0 && x != 1 {
				return 0, errFloatUnderflow
			}
			return r, nil
		}
	}
	unaryF8("ln", logf(pgLibm.Log))
	for _, n := range []string{"log", "log10"} {
		unaryEnv(n, func(l pgLibmFuncs, x float64) (float64, error) { return logf(l.Log10)(x) })
	}
	trig := func(f func(float64) float64, overflow bool) func(float64) (float64, error) {
		return func(x float64) (float64, error) {
			if math.IsNaN(x) {
				return math.NaN(), nil
			}
			if math.IsInf(x, 0) {
				return 0, errOutOfRange
			}
			r := f(x)
			if overflow && math.IsInf(r, 0) {
				return 0, errFloatOverflow
			}
			return r, nil
		}
	}
	unaryF8("sin", trig(pgLibm.Sin, true))
	unaryF8("cos", trig(pgLibm.Cos, true))
	unaryF8("tan", trig(pgLibm.Tan, false))
	inverse := func(f func(float64) float64, bounded bool) func(float64) (float64, error) {
		return func(x float64) (float64, error) {
			if math.IsNaN(x) {
				return math.NaN(), nil
			}
			if bounded && (x < -1 || x > 1) {
				return 0, errOutOfRange
			}
			r := f(x)
			if math.IsInf(r, 0) {
				return 0, errFloatOverflow
			}
			return r, nil
		}
	}
	unaryF8("asin", inverse(pgLibm.Asin, true))
	unaryF8("acos", inverse(pgLibm.Acos, true))
	unaryF8("atan", inverse(pgLibm.Atan, false))
	unaryEnv("sinh", func(l pgLibmFuncs, x float64) (float64, error) { return l.Sinh(x), nil })
	unaryEnv("cosh", func(l pgLibmFuncs, x float64) (float64, error) {
		r := l.Cosh(x)
		if r == 0 {
			return 0, errFloatUnderflow
		}
		return r, nil
	})
	unaryEnv("tanh", func(l pgLibmFuncs, x float64) (float64, error) {
		r := l.Tanh(x)
		if math.IsInf(r, 0) {
			return 0, errFloatOverflow
		}
		return r, nil
	})
	unaryEnv("asinh", func(l pgLibmFuncs, x float64) (float64, error) { return l.Asinh(x), nil })
	unaryEnv("acosh", func(l pgLibmFuncs, x float64) (float64, error) {
		if x < 1 {
			return 0, errOutOfRange
		}
		return l.Acosh(x), nil
	})
	unaryEnv("atanh", func(l pgLibmFuncs, x float64) (float64, error) {
		switch {
		case x < -1 || x > 1:
			return 0, errOutOfRange
		case x == -1:
			return math.Inf(-1), nil
		case x == 1:
			return math.Inf(1), nil
		}
		return l.Atanh(x), nil
	})
	unaryF8("degrees", func(x float64) (float64, error) { return float8Div(x, radiansPerDegree) })
	unaryF8("radians", func(x float64) (float64, error) { return float8Mul(x, radiansPerDegree) })
	registerPg("pi()", func([]any) (any, error) { return f8r(math.Pi), nil })
	registerPg("atan2(float8,float8)", func(a []any) (any, error) {
		y, x := f8(a[0]), f8(a[1])
		if math.IsNaN(x) || math.IsNaN(y) {
			return f8r(math.NaN()), nil
		}
		r := pgLibm.Atan2(y, x)
		if math.IsInf(r, 0) {
			return nil, errFloatOverflow
		}
		return f8r(r), nil
	})
	pow := func(a []any) (any, error) {
		r, err := pgPow(f8(a[0]), f8(a[1]))
		if err != nil {
			return nil, err
		}
		return f8r(r), nil
	}
	registerPg("power(float8,float8)", pow)
	registerPg("pow(float8,float8)", pow)

	// Integers.
	registerPg("abs(int4)", func(a []any) (any, error) {
		x := i4(a[0])
		if x == math.MinInt32 {
			return nil, errIntOutOfRange
		}
		if x < 0 {
			x = -x
		}
		return pgInt4(x), nil
	})
	registerPg("abs(int8)", func(a []any) (any, error) {
		x := i8(a[0])
		if x == math.MinInt64 {
			return nil, errInt8OutOfRange
		}
		if x < 0 {
			x = -x
		}
		return x, nil
	})
	registerPg("mod(int4,int4)", func(a []any) (any, error) {
		x, y := i4(a[0]), i4(a[1])
		if y == 0 {
			return nil, errDivisionByZero
		}
		if y == -1 {
			return pgInt4(0), nil
		}
		return pgInt4(x % y), nil
	})
	registerPg("mod(int8,int8)", func(a []any) (any, error) {
		x, y := i8(a[0]), i8(a[1])
		if y == 0 {
			return nil, errDivisionByZero
		}
		if y == -1 {
			return int64(0), nil
		}
		return x % y, nil
	})
	registerPg("gcd(int4,int4)", func(a []any) (any, error) {
		r, err := intGCD(int64(i4(a[0])), int64(i4(a[1])), math.MinInt32)
		if err != nil {
			return nil, errIntOutOfRange
		}
		return pgInt4(r), nil
	})
	registerPg("gcd(int8,int8)", func(a []any) (any, error) {
		r, err := intGCD(i8(a[0]), i8(a[1]), math.MinInt64)
		if err != nil {
			return nil, errInt8OutOfRange
		}
		return r, nil
	})
	registerPg("lcm(int4,int4)", func(a []any) (any, error) {
		r, err := intLCM(int64(i4(a[0])), int64(i4(a[1])), math.MinInt32, math.MaxInt32)
		if err != nil {
			return nil, errIntOutOfRange
		}
		return pgInt4(r), nil
	})
	registerPg("lcm(int8,int8)", func(a []any) (any, error) {
		r, err := intLCM(i8(a[0]), i8(a[1]), math.MinInt64, math.MaxInt64)
		if err != nil {
			return nil, errInt8OutOfRange
		}
		return r, nil
	})
	registerPg("factorial(int8)", func(a []any) (any, error) {
		n := i8(a[0])
		if n < 0 {
			return nil, pgErrorf("factorial of a negative number is undefined")
		}
		if n <= 1 {
			return numr(numOne)
		}
		if n > 32177 {
			return nil, errNumericOverflow
		}
		r := int64ToNumeric(n)
		for k := n - 1; k > 1; k-- {
			r = mulVar(r, int64ToNumeric(k), 0)
		}
		return numr(r)
	})

	// Numerics.
	registerPg("abs(numeric)", func(a []any) (any, error) {
		n := num(a[0]).clone()
		switch n.sign {
		case numNInf:
			n.sign = numPInf
		case numNeg:
			n.sign = numPos
		}
		return n, nil
	})
	registerPg("sign(numeric)", func(a []any) (any, error) {
		n := num(a[0])
		if n.isNaN() {
			return numNaNVal, nil
		}
		switch n.signum() {
		case 1:
			return numr(numOne)
		case -1:
			return numr(numMinus1)
		}
		return numr(numZero)
	})
	roundOrTrunc := func(trunc bool) pgImpl {
		return func(a []any) (any, error) {
			n := num(a[0])
			scale := 0
			if len(a) == 2 {
				scale = int(i4(a[1]))
			}
			if n.isSpecial() {
				return n, nil
			}
			scale = max(scale, -numericMaxResult)
			scale = min(scale, numericMaxResult)
			v := n.clone()
			if trunc {
				v.trunc(scale)
			} else {
				v.round(scale)
			}
			if scale < 0 {
				v.dscale = 0
			}
			return numr(v)
		}
	}
	registerPg("round(numeric,int4)", roundOrTrunc(false))
	registerPg("round(numeric)", roundOrTrunc(false))
	registerPg("trunc(numeric,int4)", roundOrTrunc(true))
	registerPg("trunc(numeric)", roundOrTrunc(true))
	ceilFloor := func(f func(pgNumeric) pgNumeric) pgImpl {
		return func(a []any) (any, error) {
			n := num(a[0])
			if n.isSpecial() {
				return n, nil
			}
			return numr(f(n))
		}
	}
	registerPg("ceil(numeric)", ceilFloor(ceilVar))
	registerPg("ceiling(numeric)", ceilFloor(ceilVar))
	registerPg("floor(numeric)", ceilFloor(floorVar))
	registerPg("sqrt(numeric)", func(a []any) (any, error) {
		n := num(a[0])
		if n.isSpecial() {
			if n.sign == numNInf {
				return nil, errSqrtNegative
			}
			return n, nil
		}
		sweight := (n.weight+1)*decDigits/2 - 1
		rscale := numericMinSigDigit - sweight
		rscale = max(rscale, n.dscale, numericMinDisplay)
		rscale = min(rscale, numericMaxDisplay)
		r, err := sqrtVar(n, rscale)
		if err != nil {
			return nil, err
		}
		return numr(r)
	})
	registerPg("exp(numeric)", func(a []any) (any, error) {
		n := num(a[0])
		if n.isSpecial() {
			if n.sign == numNInf {
				return numr(numZero)
			}
			return n, nil
		}
		val := float64(n.toFloat64() * 0.434294481903252)
		val = max(val, -numericMaxResult)
		val = min(val, numericMaxResult)
		rscale := numericMinSigDigit - int(val)
		rscale = max(rscale, n.dscale, numericMinDisplay)
		rscale = min(rscale, numericMaxDisplay)
		r, err := expVar(n, rscale)
		if err != nil {
			return nil, err
		}
		return numr(r)
	})
	registerPg("ln(numeric)", func(a []any) (any, error) {
		n := num(a[0])
		if n.isSpecial() {
			if n.sign == numNInf {
				return nil, errLogNegative
			}
			return n, nil
		}
		rscale := numericMinSigDigit - estimateLnDweight(n)
		rscale = max(rscale, n.dscale, numericMinDisplay)
		rscale = min(rscale, numericMaxDisplay)
		r, err := lnVar(n, rscale)
		if err != nil {
			return nil, err
		}
		return numr(r)
	})
	logNumeric := func(base, n pgNumeric) (any, error) {
		if base.isSpecial() || n.isSpecial() {
			if base.isNaN() || n.isNaN() {
				return numNaNVal, nil
			}
			s1, s2 := base.signum(), n.signum()
			if s1 < 0 || s2 < 0 {
				return nil, errLogNegative
			}
			if s1 == 0 || s2 == 0 {
				return nil, errLogZero
			}
			if base.sign == numPInf {
				if n.sign == numPInf {
					return numNaNVal, nil
				}
				return numr(numZero)
			}
			return numPInfVal, nil
		}
		r, err := logVar(base, n)
		if err != nil {
			return nil, err
		}
		return numr(r)
	}
	registerPg("log(numeric,numeric)", func(a []any) (any, error) { return logNumeric(num(a[0]), num(a[1])) })
	ten := int64ToNumeric(10)
	registerPg("log(numeric)", func(a []any) (any, error) { return logNumeric(ten, num(a[0])) })
	registerPg("log10(numeric)", func(a []any) (any, error) { return logNumeric(ten, num(a[0])) })
	numPow := func(a []any) (any, error) { return numericPower(num(a[0]), num(a[1])) }
	registerPg("power(numeric,numeric)", numPow)
	registerPg("pow(numeric,numeric)", numPow)
	registerPg("mod(numeric,numeric)", func(a []any) (any, error) {
		x, y := num(a[0]), num(a[1])
		if x.isSpecial() || y.isSpecial() {
			if x.isNaN() || y.isNaN() {
				return numNaNVal, nil
			}
			if x.isInf() {
				if y.signum() == 0 {
					return nil, errDivisionByZero
				}
				return numNaNVal, nil
			}
			return x, nil
		}
		r, err := modVar(x, y)
		if err != nil {
			return nil, err
		}
		return numr(r)
	})
	registerPg("div(numeric,numeric)", func(a []any) (any, error) {
		x, y := num(a[0]), num(a[1])
		if x.isSpecial() || y.isSpecial() {
			return numericSpecialDiv(x, y)
		}
		r, err := divVar(x, y, 0, false)
		if err != nil {
			return nil, err
		}
		return numr(r)
	})
	registerPg("gcd(numeric,numeric)", func(a []any) (any, error) {
		x, y := num(a[0]), num(a[1])
		if x.isSpecial() || y.isSpecial() {
			return numNaNVal, nil
		}
		r, err := gcdVar(x, y)
		if err != nil {
			return nil, err
		}
		return numr(r)
	})
	registerPg("lcm(numeric,numeric)", func(a []any) (any, error) {
		x, y := num(a[0]), num(a[1])
		if x.isSpecial() || y.isSpecial() {
			return numNaNVal, nil
		}
		var r pgNumeric
		if len(x.digits) == 0 || len(y.digits) == 0 {
			r = pgNumeric{}
		} else {
			g, err := gcdVar(x, y)
			if err != nil {
				return nil, err
			}
			q, err := divVar(x, g, 0, false)
			if err != nil {
				return nil, err
			}
			r = mulVar(y, q, y.dscale)
			r.sign = numPos
		}
		r.dscale = max(x.dscale, y.dscale)
		return numr(r)
	})
	registerPg("scale(numeric)", func(a []any) (any, error) {
		n := num(a[0])
		if n.isSpecial() {
			return nil, nil
		}
		return pgInt4(n.dscale), nil
	})
	registerPg("min_scale(numeric)", func(a []any) (any, error) {
		n := num(a[0])
		if n.isSpecial() {
			return nil, nil
		}
		return pgInt4(n.minScale()), nil
	})
	registerPg("trim_scale(numeric)", func(a []any) (any, error) {
		n := num(a[0])
		if n.isSpecial() {
			return n, nil
		}
		r := n.clone()
		r.dscale = n.minScale()
		return numr(r)
	})
}

// numericSpecialDiv is numeric_div_trunc's NaN and infinity handling.
func numericSpecialDiv(x, y pgNumeric) (any, error) {
	if x.isNaN() || y.isNaN() {
		return numNaNVal, nil
	}
	if x.isInf() {
		if y.isSpecial() {
			return numNaNVal, nil
		}
		switch y.signum() {
		case 0:
			return nil, errDivisionByZero
		case 1:
			return pgNumeric{sign: x.sign}, nil
		}
		if x.sign == numPInf {
			return numNInfVal, nil
		}
		return numPInfVal, nil
	}
	return numr(numZero)
}

func gcdVar(a, b pgNumeric) (pgNumeric, error) {
	resDscale := max(a.dscale, b.dscale)
	cmp := cmpAbs(a, b)
	if cmp < 0 {
		a, b = b, a
	}
	if cmp == 0 || len(b.digits) == 0 {
		r := a.clone()
		r.sign = numPos
		r.dscale = resDscale
		return r, nil
	}
	tmp, result := a.clone(), b.clone()
	for {
		m, err := modVar(tmp, result)
		if err != nil {
			return pgNumeric{}, err
		}
		if len(m.digits) == 0 {
			break
		}
		tmp, result = result, m
	}
	result.sign = numPos
	result.dscale = resDscale
	return result, nil
}

// numericPower is numeric_power.
func numericPower(x, y pgNumeric) (any, error) {
	if x.isSpecial() || y.isSpecial() {
		if x.isNaN() {
			if !y.isSpecial() && cmpVar(y, numZero) == 0 {
				return numr(numOne)
			}
			return numNaNVal, nil
		}
		if y.isNaN() {
			if !x.isSpecial() && cmpVar(x, numOne) == 0 {
				return numr(numOne)
			}
			return numNaNVal, nil
		}
		s1, s2 := x.signum(), y.signum()
		if s1 == 0 && s2 < 0 {
			return nil, pgErrorf("zero raised to a negative power is undefined")
		}
		if s1 < 0 && !y.isIntegral() {
			return nil, pgErrorf("a negative number raised to a non-integer power yields a complex result")
		}
		if !x.isSpecial() && cmpVar(x, numOne) == 0 {
			return numr(numOne)
		}
		if s2 == 0 {
			return numr(numOne)
		}
		if s1 == 0 && s2 > 0 {
			return numr(numZero)
		}
		if y.isInf() {
			var absGtOne bool
			if x.isSpecial() {
				absGtOne = true
			} else {
				if cmpVar(x, numMinus1) == 0 {
					return numr(numOne)
				}
				ax := x.clone()
				ax.sign = numPos
				absGtOne = cmpVar(ax, numOne) > 0
			}
			if absGtOne == (s2 > 0) {
				return numPInfVal, nil
			}
			return numr(numZero)
		}
		if x.sign == numPInf {
			if s2 > 0 {
				return numPInfVal, nil
			}
			return numr(numZero)
		}
		if s2 < 0 {
			return numr(numZero)
		}
		if len(y.digits) > 0 && len(y.digits) == y.weight+1 && y.digits[len(y.digits)-1]&1 != 0 {
			return numNInfVal, nil
		}
		return numPInfVal, nil
	}
	if x.signum() == 0 && y.signum() < 0 {
		return nil, pgErrorf("zero raised to a negative power is undefined")
	}
	r, err := powerVar(x, y)
	if err != nil {
		return nil, err
	}
	return numr(r)
}

// checkFloat is float.c's overflow and underflow checks on a result.
func checkFloat(r float64, inputInf, inputZero bool) (float64, error) {
	if math.IsInf(r, 0) && !inputInf {
		return 0, errFloatOverflow
	}
	if r == 0 && !inputZero {
		return 0, errFloatUnderflow
	}
	return r, nil
}

func float8Div(a, b float64) (float64, error) {
	if b == 0 && !math.IsNaN(a) {
		return 0, errDivisionByZero
	}
	r := a / b
	if math.IsInf(r, 0) && !math.IsInf(a, 0) {
		return 0, errFloatOverflow
	}
	if r == 0 && a != 0 && !math.IsInf(b, 0) {
		return 0, errFloatUnderflow
	}
	return r, nil
}

func float8Mul(a, b float64) (float64, error) {
	r := float64(a * b)
	if math.IsInf(r, 0) && !math.IsInf(a, 0) && !math.IsInf(b, 0) {
		return 0, errFloatOverflow
	}
	if r == 0 && a != 0 && b != 0 {
		return 0, errFloatUnderflow
	}
	return r, nil
}

// pgPow is dpow.
func pgPow(x, y float64) (float64, error) {
	if math.IsNaN(x) {
		if math.IsNaN(y) || y != 0 {
			return math.NaN(), nil
		}
		return 1, nil
	}
	if math.IsNaN(y) {
		if x != 1 {
			return math.NaN(), nil
		}
		return 1, nil
	}
	if x == 0 && y < 0 {
		return 0, pgErrorf("zero raised to a negative power is undefined")
	}
	if x < 0 && math.Floor(y) != y {
		return 0, pgErrorf("a negative number raised to a non-integer power yields a complex result")
	}
	if math.IsInf(y, 0) {
		absx := math.Abs(x)
		switch {
		case absx == 1:
			return 1, nil
		case y > 0:
			if absx > 1 {
				return y, nil
			}
			return 0, nil
		}
		if absx > 1 {
			return 0, nil
		}
		return -y, nil
	}
	if math.IsInf(x, 0) {
		switch {
		case y == 0:
			return 1, nil
		case x > 0:
			if y > 0 {
				return x, nil
			}
			return 0, nil
		}
		halfy := y / 2
		odd := math.Floor(halfy) != halfy
		if y > 0 {
			if odd {
				return x, nil
			}
			return -x, nil
		}
		if odd {
			return math.Copysign(0, -1), nil
		}
		return 0, nil
	}
	r := pgLibm.Pow(x, y)
	if math.IsNaN(r) {
		if x == 0 {
			return 0, nil
		}
		absx := math.Abs(x)
		switch {
		case absx == 1:
			return 1, nil
		case (y >= 0 && absx > 1) || (y < 0 && absx < 1):
			return 0, errFloatOverflow
		}
		return 0, errFloatUnderflow
	}
	if math.IsInf(r, 0) {
		return 0, errFloatOverflow
	}
	if r == 0 && x != 0 {
		return 0, errFloatUnderflow
	}
	return r, nil
}

func intGCD(a, b, minVal int64) (int64, error) {
	if a == minVal || b == minVal {
		if a == 0 || b == 0 {
			return 0, errIntOutOfRange
		}
		if a == minVal && b == minVal {
			return 0, errIntOutOfRange
		}
	}
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a, nil
}

func intLCM(a, b, minVal, maxVal int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	g, err := intGCD(a, b, minVal)
	if err != nil {
		return 0, err
	}
	q := a / g
	r := q * b
	if q != 0 && r/q != b {
		return 0, errIntOutOfRange
	}
	if r < 0 {
		r = -r
	}
	if r > maxVal || r < 0 {
		return 0, errIntOutOfRange
	}
	return r, nil
}
