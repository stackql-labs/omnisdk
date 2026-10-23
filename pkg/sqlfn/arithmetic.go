package sqlfn

import (
	"math"
	"math/big"
)

func sqliteArithmetic() []Func {
	var functions []Func
	for _, operator := range []string{"+", "-", "*", "/", "%"} {
		functions = append(functions, NewScalar(operator, 2, 2, func(args []any) (any, error) {
			return sqliteBinaryArithmetic(operator, args[0], args[1]), nil
		}))
	}
	return functions
}

// Arithmetic retains integer precision until an operation overflows. Numeric
// text keeps a decimal/exponent form REAL, unlike CAST(... AS NUMERIC).
func arithmeticNumber(value any) (class, any) {
	c, value := norm(value)
	if c == classInteger || c == classReal || c == classNull {
		return c, value
	}
	text := sqlText(value)
	real, flags := sqliteAtoFRaw(text)
	integer, result := atoi64(text)
	if flags&2 == 0 && result < 2 {
		return classInteger, integer
	}
	return classReal, real
}

func sqliteBinaryArithmetic(operator string, left, right any) any {
	originalLeft, originalRight := left, right
	lc, left := arithmeticNumber(left)
	rc, right := arithmeticNumber(right)
	if lc == classNull || rc == classNull {
		return nil
	}
	if lc == classInteger && rc == classInteger {
		x, y := left.(int64), right.(int64)
		if (operator == "/" || operator == "%") && y == 0 {
			return nil
		}
		var result big.Int
		switch operator {
		case "+":
			result.Add(big.NewInt(x), big.NewInt(y))
		case "-":
			result.Sub(big.NewInt(x), big.NewInt(y))
		case "*":
			result.Mul(big.NewInt(x), big.NewInt(y))
		case "/":
			if x != math.MinInt64 || y != -1 {
				return x / y
			}
		case "%":
			return x % y
		}
		if operator != "/" && result.IsInt64() {
			return result.Int64()
		}
	}
	x, y := sqlReal(left), sqlReal(right)
	if (operator == "/" || operator == "%") && y == 0 {
		return nil
	}
	var result float64
	switch operator {
	case "+":
		result = x + y
	case "-":
		result = x - y
	case "*":
		result = x * y
	case "/":
		result = x / y
	case "%":
		denominator := sqlInt(originalRight)
		if denominator == 0 {
			return nil
		}
		result = float64(sqlInt(originalLeft) % denominator)
	}
	if math.IsNaN(result) {
		return nil
	}
	return result
}
