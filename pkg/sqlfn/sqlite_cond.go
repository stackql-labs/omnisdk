package sqlfn

import (
	"bytes"
	"strings"
)

// sqliteCompare orders two non-NULL values as SQLite does with no affinity and BINARY collation:
// numbers before text before BLOBs; numbers by value, an INTEGER against a REAL exactly; text and
// BLOBs by their bytes.
func sqliteCompare(a, b any) int {
	ca, xa := norm(a)
	cb, xb := norm(b)
	rank := func(c class) int {
		switch c {
		case classInteger, classReal:
			return 1
		case classText:
			return 2
		case classBlob:
			return 3
		}
		return 0
	}
	if ra, rb := rank(ca), rank(cb); ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch ca {
	case classInteger, classReal:
		return compareNumbers(ca, xa, cb, xb)
	case classText:
		return strings.Compare(xa.(string), xb.(string))
	case classBlob:
		return bytes.Compare(xa.([]byte), xb.([]byte))
	}
	return 0
}

// compareNumbers orders an INTEGER or REAL against another, exactly: an INTEGER is never rounded to a
// REAL before the comparison decides.
func compareNumbers(ca class, xa any, cb class, xb any) int {
	if ca == classInteger && cb == classInteger {
		return cmpInt(xa.(int64), xb.(int64))
	}
	if ca == classReal && cb == classReal {
		return cmpFloat(xa.(float64), xb.(float64))
	}
	if ca == classInteger {
		return -compareRealInt(xb.(float64), xa.(int64))
	}
	return compareRealInt(xa.(float64), xb.(int64))
}

func compareRealInt(r float64, i int64) int {
	switch {
	case r < -9223372036854775808.0:
		return -1
	case r >= 9223372036854775808.0:
		return 1
	}
	t := int64(r)
	if c := cmpInt(t, i); c != 0 {
		return c
	}
	return cmpFloat(r, float64(t))
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// truthy is a value as SQLite reads it for a condition: NULL is not true, a number is true when not
// zero, text and BLOBs by the number they lead with.
func truthy(v any) (bool, bool) {
	c, x := norm(v)
	switch c {
	case classNull:
		return false, false
	case classInteger:
		return x.(int64) != 0, true
	case classReal:
		return x.(float64) != 0, true
	}
	return sqlReal(x) != 0, true
}

func sqliteConditional() []Func {
	return []Func{
		NewScalar("coalesce", 2, -1, func(a []any) (any, error) {
			for _, v := range a {
				if !isNull(v) {
					return v, nil
				}
			}
			return nil, nil
		}),
		NewScalar("ifnull", 2, 2, func(a []any) (any, error) {
			if !isNull(a[0]) {
				return a[0], nil
			}
			return a[1], nil
		}),
		NewScalar("nullif", 2, 2, func(a []any) (any, error) {
			if !isNull(a[0]) && !isNull(a[1]) && sqliteCompare(a[0], a[1]) == 0 {
				return nil, nil
			}
			return a[0], nil
		}),
		NewScalar("iif", 2, -1, iifFn),
		NewScalar("if", 2, -1, iifFn),
		NewScalar("max", 2, -1, extremum(1)),
		NewScalar("min", 2, -1, extremum(-1)),
		NewScalar("subtype", 1, 1, func([]any) (any, error) { return int64(0), nil }),
	}
}

// iifFn is iif(C1, V1, C2, V2, …[, ELSE]): the value after the first true condition, ELSE or NULL.
func iifFn(a []any) (any, error) {
	i := 0
	for ; i+1 < len(a); i += 2 {
		if t, _ := truthy(a[i]); t {
			return a[i+1], nil
		}
	}
	if i < len(a) {
		return a[i], nil
	}
	return nil, nil
}

// extremum is multi-argument max (dir 1) or min (dir -1): NULL if any argument is NULL, otherwise the
// greatest or least, ties broken as SQLite's minmaxFunc breaks them — max keeps the earlier of equal
// values, min takes the later.
func extremum(dir int) func([]any) (any, error) {
	return func(a []any) (any, error) {
		best := a[0]
		if isNull(best) {
			return nil, nil
		}
		for _, v := range a[1:] {
			if isNull(v) {
				return nil, nil
			}
			c := sqliteCompare(best, v)
			if (dir > 0 && c < 0) || (dir < 0 && c >= 0) {
				best = v
			}
		}
		return best, nil
	}
}
