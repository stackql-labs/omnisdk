package sqlfn

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// SQLite's math functions (func.c, built with SQLITE_ENABLE_MATH_FUNCTIONS as stackql's SQLite is).

// errIntegerOverflow is SQLite's "integer overflow", raised by abs(-9223372036854775808).
var errIntegerOverflow = errors.New("integer overflow")

func sqliteMath() []Func {
	unary := func(name string, f func(float64) float64) Func {
		return NewScalar(name, 1, 1, func(a []any) (any, error) { return math1(a[0], f), nil })
	}
	binary := func(name string, f func(x, y float64) float64) Func {
		return NewScalar(name, 2, 2, func(a []any) (any, error) { return math2(a[0], a[1], f), nil })
	}
	return []Func{
		NewScalar("abs", 1, 1, func(a []any) (any, error) {
			c, x := norm(a[0])
			switch c {
			case classNull:
				return nil, nil
			case classInteger:
				i := x.(int64)
				if i == math.MinInt64 {
					return nil, errIntegerOverflow
				}
				if i < 0 {
					i = -i
				}
				return i, nil
			}
			return math.Abs(sqlReal(x)), nil
		}),
		NewScalar("ceil", 1, 1, ceilingFn(math.Ceil)),
		NewScalar("ceiling", 1, 1, ceilingFn(math.Ceil)),
		NewScalar("floor", 1, 1, ceilingFn(math.Floor)),
		NewScalar("trunc", 1, 1, ceilingFn(math.Trunc)),
		unary("acos", math.Acos), unary("acosh", math.Acosh), unary("asin", math.Asin),
		unary("asinh", math.Asinh), unary("atan", math.Atan), unary("atanh", math.Atanh),
		unary("cos", math.Cos), unary("cosh", math.Cosh), unary("exp", math.Exp),
		NewScalar("ln", 1, 1, func(a []any) (any, error) { return logFn(a, math.Log), nil }),
		NewScalar("log10", 1, 1, func(a []any) (any, error) { return logFn(a, math.Log10), nil }),
		NewScalar("log2", 1, 1, func(a []any) (any, error) { return logFn(a, math.Log2), nil }),
		unary("sin", math.Sin), unary("sinh", math.Sinh), unary("sqrt", math.Sqrt),
		unary("tan", math.Tan), unary("tanh", math.Tanh),
		unary("degrees", func(x float64) float64 { return x * (180.0 / math.Pi) }),
		unary("radians", func(x float64) float64 { return x * (math.Pi / 180.0) }),
		binary("atan2", math.Atan2),
		binary("pow", math.Pow), binary("power", math.Pow),
		binary("mod", math.Mod),
		NewScalar("log", 1, 2, func(a []any) (any, error) { return logFn(a, math.Log10), nil }),
		NewScalar("pi", 0, 0, func([]any) (any, error) { return math.Pi, nil }),
		NewScalar("sign", 1, 1, func(a []any) (any, error) {
			c, x := numericClass(a[0])
			switch c {
			case classInteger:
				return signOf(float64(x.(int64))), nil
			case classReal:
				return signOf(x.(float64)), nil
			}
			return nil, nil
		}),
		NewScalar("round", 1, 2, roundFn),
	}
}

func signOf(f float64) int64 {
	switch {
	case f < 0:
		return -1
	case f > 0:
		return 1
	}
	return 0
}

// math1 is SQLite's math1Func: a number in, a REAL out, NULL for anything not a number and for any
// result that is not a finite real.
func math1(v any, f func(float64) float64) any {
	x, ok := mathArg(v)
	if !ok {
		return nil
	}
	return finite(f(x))
}

func math2(a, b any, f func(x, y float64) float64) any {
	x, ok := mathArg(a)
	if !ok {
		return nil
	}
	y, ok := mathArg(b)
	if !ok {
		return nil
	}
	return finite(f(x, y))
}

func mathArg(v any) (float64, bool) {
	c, x := numericClass(v)
	switch c {
	case classInteger:
		return float64(x.(int64)), true
	case classReal:
		return x.(float64), true
	}
	return 0, false
}

// finite is a result SQLite returns: NULL for NaN; an infinity is returned as one.
func finite(f float64) any {
	if math.IsNaN(f) {
		return nil
	}
	return f
}

// ceilingFn is ceil, floor and trunc: an INTEGER unchanged, a REAL rounded, NULL otherwise.
func ceilingFn(f func(float64) float64) func([]any) (any, error) {
	return func(a []any) (any, error) {
		c, x := numericClass(a[0])
		switch c {
		case classInteger:
			return x, nil
		case classReal:
			return f(x.(float64)), nil
		}
		return nil, nil
	}
}

// roundFn is SQLite's roundFunc: a REAL rounded half away from zero to N places (N clamped to 0..30),
// by the same decimal rendering SQLite uses, so a value whose binary form falls short of the half
// rounds down — round(1.005, 2) is 1.0.
func roundFn(a []any) (any, error) {
	n := int64(0)
	if len(a) == 2 {
		if c, _ := norm(a[1]); c == classNull {
			return nil, nil
		}
		n = sqlInt(a[1])
		if n > 30 {
			n = 30
		}
		if n < 0 {
			n = 0
		}
	}
	if c, _ := norm(a[0]); c == classNull {
		return nil, nil
	}
	r := sqlReal(a[0])
	// Values beyond 2^52 have no fractional part to round.
	if r < -4503599627370496.0 || r > 4503599627370496.0 {
		return r, nil
	}
	if n == 0 {
		if r < 0 {
			return float64(int64(r - 0.5)), nil
		}
		return float64(int64(r + 0.5)), nil
	}
	return roundDecimal(r, int(n)), nil
}

// roundDecimal rounds r half away from zero at n decimal places, from its exact decimal expansion.
func roundDecimal(r float64, n int) float64 {
	s := strconv.FormatFloat(math.Abs(r), 'f', n+30, 64)
	intPart, frac, _ := strings.Cut(s, ".")
	keep := intPart + frac[:n]
	digits := []byte(keep)
	if frac[n] >= '5' {
		i := len(digits) - 1
		for ; i >= 0; i-- {
			if digits[i] == '9' {
				digits[i] = '0'
				continue
			}
			digits[i]++
			break
		}
		if i < 0 {
			digits = append([]byte{'1'}, digits...)
		}
	}
	str := string(digits)
	whole := str[:len(str)-n]
	if whole == "" {
		whole = "0"
	}
	out, _ := strconv.ParseFloat(whole+"."+str[len(str)-n:], 64)
	if r < 0 {
		out = -out
	}
	return out
}

// logFn is SQLite's logFunc. One argument: NULL unless it is a positive number, else single(x). Two:
// log(B, X) is ln(X)/ln(B), NULL unless B is a number greater than 1 and X is positive. SQLite checks
// the first argument's type twice and never the second's, which it reads as a REAL whatever it is —
// so log(2, '8abc') is 3.
func logFn(a []any, single func(float64) float64) any {
	x, ok := mathArg(a[0])
	if !ok || x <= 0 {
		return nil
	}
	if len(a) == 1 {
		return finite(single(x))
	}
	b := math.Log(x)
	if b <= 0 {
		return nil
	}
	y := sqlReal(a[1])
	if y <= 0 {
		return nil
	}
	return finite(math.Log(y) / b)
}
