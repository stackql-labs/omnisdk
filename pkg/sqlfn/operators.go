package sqlfn

import (
	"math"
	"strings"
)

// The operators a query plan calls as functions: x || y, CAST(x AS type), x IS NULL and
// x BETWEEN a AND b, each as its dialect evaluates it. Comparison here takes no account of column
// affinity (SQLite) or of operator overloading beyond the built-in types (Postgres).

func sqliteOperators() []Func {
	return []Func{
		// OP_Concat: NULL if any operand is; otherwise each operand's TEXT.
		NewScalar("||", 2, -1, func(a []any) (any, error) {
			var b strings.Builder
			for _, v := range a {
				if isNull(v) {
					return nil, nil
				}
				b.Write(sqlBytes(v))
			}
			return b.String(), nil
		}),
		NewScalar("cast", 2, 2, func(a []any) (any, error) { return sqliteCast(a[0], sqlText(a[1])), nil }),
		NewScalar("is_null", 1, 1, func(a []any) (any, error) { return boolInt(isNull(a[0])), nil }),
		NewScalar("between", 3, 3, func(a []any) (any, error) {
			cmp := func(x, y any) *int {
				if isNull(x) || isNull(y) {
					return nil
				}
				c := sqliteCompare(x, y)
				return &c
			}
			ge, le := cmp(a[0], a[1]), cmp(a[0], a[2])
			return sqlAnd(ge, func(c int) bool { return c >= 0 }, le, func(c int) bool { return c <= 0 }, func(b bool) any { return boolInt(b) }), nil
		}),
	}
}

// sqlAnd is SQL's three-valued AND of two comparisons, nil being NULL.
func sqlAnd(x *int, px func(int) bool, y *int, py func(int) bool, out func(bool) any) any {
	if x != nil && !px(*x) || y != nil && !py(*y) {
		return out(false)
	}
	if x == nil || y == nil {
		return nil
	}
	return out(true)
}

// SQLite's type affinities (sqlite3AffinityType).
const (
	affBlob = iota
	affText
	affNumeric
	affInteger
	affReal
)

// sqliteAffinity is sqlite3AffinityType: the affinity a declared type name gives.
func sqliteAffinity(name string) int {
	aff := affNumeric
	var h uint32
	for i := 0; i < len(name); i++ {
		h = h<<8 + uint32(asciiLower(name[i]))
		switch {
		case h == 'c'<<24|'h'<<16|'a'<<8|'r', h == 'c'<<24|'l'<<16|'o'<<8|'b', h == 't'<<24|'e'<<16|'x'<<8|'t':
			aff = affText
		case h == 'b'<<24|'l'<<16|'o'<<8|'b' && (aff == affNumeric || aff == affReal):
			aff = affBlob
		case (h == 'r'<<24|'e'<<16|'a'<<8|'l' || h == 'f'<<24|'l'<<16|'o'<<8|'a' || h == 'd'<<24|'o'<<16|'u'<<8|'b') && aff == affNumeric:
			aff = affReal
		case h&0x00FFFFFF == 'i'<<16|'n'<<8|'t':
			return affInteger
		}
	}
	return aff
}

// sqliteCast is sqlite3VdbeMemCast.
func sqliteCast(v any, typ string) any {
	c, x := norm(v)
	if c == classNull {
		return nil
	}
	switch sqliteAffinity(typ) {
	case affBlob:
		if c == classBlob {
			return x
		}
		return []byte(sqlText(x))
	case affText:
		return sqlText(x)
	case affInteger:
		return sqlInt(x)
	case affReal:
		return sqlReal(x)
	}
	// NUMERIC: sqlite3VdbeMemNumerify.
	if c == classInteger || c == classReal {
		return x
	}
	s := sqlText(x)
	r, rc := sqliteAtoFRaw(s)
	if ix, irc := atoi64(s); rc&2 == 0 && irc < 2 {
		return ix
	}
	if ix := sqliteRealToI64(r); sqliteRealSameAsInt(r, ix) {
		return ix
	}
	return r
}

func sqliteRealToI64(r float64) int64 {
	switch {
	case r < -9223372036854774784.0:
		return -1 << 63
	case r > 9223372036854774784.0:
		return 1<<63 - 1
	}
	return int64(r)
}

func sqliteRealSameAsInt(r float64, i int64) bool {
	return r == 0 || r == float64(i) && i >= -2251799813685248 && i < 2251799813685248
}

func pgOperators(env pgEnv) []Func {
	return []Func{
		NewScalar("||", 2, -1, func(a []any) (any, error) {
			acc := a[0]
			for _, v := range a[1:] {
				r, err := pgConcat(env, acc, v)
				if err != nil {
					return nil, err
				}
				acc = r
			}
			return acc, nil
		}),
		NewScalar("cast", 2, 2, func(a []any) (any, error) {
			if a[0] == nil {
				return nil, nil
			}
			return pgExplicitCast(env, a[0], pgTextOrEmpty(a[1]))
		}),
		NewScalar("is_null", 1, 1, func(a []any) (any, error) { return a[0] == nil, nil }),
		NewScalar("between", 3, 3, func(a []any) (any, error) {
			ge, err := pgCompareNullable(env, a[0], a[1], ">=")
			if err != nil {
				return nil, err
			}
			le, err := pgCompareNullable(env, a[0], a[2], "<=")
			if err != nil {
				return nil, err
			}
			return sqlAnd(ge, func(c int) bool { return c >= 0 }, le, func(c int) bool { return c <= 0 },
				func(b bool) any { return b }), nil
		}),
	}
}

// pgOperandType is an operand's type for operator resolution: a NULL is an untyped literal.
func pgOperandType(v any) string {
	if v == nil {
		return "unknown"
	}
	return pgTypeNameOf(pgNormalize(v))
}

// pgConcat is the || operator Postgres resolves for the operands' types: text concatenation when
// either side is text or untyped and the other is not an array (textcat, textanycat, anytextcat),
// jsonb_concat between jsonb values, and array_cat, array_append or array_prepend for text arrays.
func pgConcat(env pgEnv, x, y any) (any, error) {
	tx, ty := pgOperandType(x), pgOperandType(y)
	isArr := func(t string) bool { return t == "_text" || t == "_int4" }
	textual := func(t string) bool { return t == "text" || t == "unknown" }
	switch {
	case tx == "_text" && (ty == "_text" || ty == "unknown"), ty == "_text" && tx == "unknown":
		if x == nil || y == nil {
			if x == nil {
				return arrOrNil(env, y, ty)
			}
			return arrOrNil(env, x, tx)
		}
		ax, err := pgArrayArg(env, x, tx)
		if err != nil {
			return nil, err
		}
		ay, err := pgArrayArg(env, y, ty)
		if err != nil {
			return nil, err
		}
		return append(append(pgTextArr{}, ax...), ay...), nil
	case tx == "_text" && ty == "text":
		e := pgElem(y)
		return append(append(pgTextArr{}, pgArrOrEmpty(x)...), e), nil
	case tx == "text" && ty == "_text":
		e := pgElem(x)
		return append(pgTextArr{e}, pgArrOrEmpty(y)...), nil
	case tx == "jsonb" && (ty == "jsonb" || ty == "unknown"), ty == "jsonb" && tx == "unknown":
		if x == nil || y == nil {
			return nil, nil
		}
		jx, err := pgCoerce(env, x, "jsonb", nil, nil)
		if err != nil {
			return nil, err
		}
		jy, err := pgCoerce(env, y, "jsonb", nil, nil)
		if err != nil {
			return nil, err
		}
		return jbConcat(jsonbValueOf(jx), jsonbValueOf(jy)), nil
	case textual(tx) && !isArr(ty), textual(ty) && !isArr(tx):
		if x == nil || y == nil {
			return nil, nil
		}
		sx, _ := pgTextOf(pgNormalize(x))
		sy, _ := pgTextOf(pgNormalize(y))
		return sx + sy, nil
	}
	return nil, pgErrorf("operator does not exist: %s || %s", pgDisplayName(tx), pgDisplayName(ty))
}

func arrOrNil(env pgEnv, v any, t string) (any, error) {
	// array_cat is not strict: a NULL operand leaves the other array.
	if v == nil {
		return nil, nil
	}
	return pgArrayArg(env, v, t)
}

func pgArrayArg(_ pgEnv, v any, t string) (pgTextArr, error) {
	if t == "unknown" {
		a, err := pgTextArrayIn(pgTextOrEmpty(v))
		if err != nil {
			return nil, err
		}
		return a.(pgTextArr), nil
	}
	return v.(pgTextArr), nil
}

func pgArrOrEmpty(v any) pgTextArr {
	if v == nil {
		return pgTextArr{}
	}
	return v.(pgTextArr)
}

func pgElem(v any) *string {
	if v == nil {
		return nil
	}
	s := v.(string)
	return &s
}

// jbConcat is jsonb_concat (IteratorConcat): objects merge, the right operand's keys winning;
// otherwise both sides' elements form an array, a scalar or an object being one element.
func jbConcat(x, y jbValue) any {
	elems := func(v jbValue) []jbValue {
		if v.kind == jbArray {
			return v.arr
		}
		v.raw = false
		return []jbValue{v}
	}
	if (x.kind == jbObject) == (y.kind == jbObject) {
		if len(x.keys)+len(x.arr) == 0 && !x.raw && !y.raw {
			return jbResult(y)
		}
		if len(y.keys)+len(y.arr) == 0 && !y.raw && !x.raw {
			return jbResult(x)
		}
	}
	if x.kind == jbObject && y.kind == jbObject {
		o := jbValue{kind: jbObject, keys: append(append([]string{}, x.keys...), y.keys...),
			vals: append(append([]jbValue{}, x.vals...), y.vals...)}
		return jbResult(uniqueify(o))
	}
	return jbResult(jbValue{kind: jbArray, arr: append(append([]jbValue{}, elems(x)...), elems(y)...)})
}

// pgCompareNullable is a comparison operator over operands that may be NULL: the operator must
// exist for their types whatever the values, and a NULL operand makes the comparison NULL.
func pgCompareNullable(env pgEnv, x, y any, op string) (*int, error) {
	tx, ty := pgOperandType(x), pgOperandType(y)
	if err := pgComparable(tx, ty, op); err != nil {
		return nil, err
	}
	if x == nil || y == nil {
		return nil, nil
	}
	c, err := pgCompare(env, x, y)
	return &c, err
}

// pgComparable reports whether Postgres has an ordering operator between the two types: within
// the numeric types, the string types, booleans and text arrays, an untyped literal taking the other
// side's type.
func pgComparable(tx, ty, op string) error {
	category := func(t string) string {
		switch t {
		case "int4", "int8", "numeric", "float8":
			return "N"
		case "text":
			return "S"
		case "bool":
			return "B"
		case "_text":
			return "A"
		case "unknown":
			return "?"
		}
		return ""
	}
	cx, cy := category(tx), category(ty)
	switch {
	case cx == "?" && cy == "?", cx == "?" && cy != "", cy == "?" && cx != "", cx != "" && cx == cy:
		return nil
	}
	return pgErrorf("operator does not exist: %s %s %s", pgDisplayName(tx), op, pgDisplayName(ty))
}

// pgTypeNamed is the catalog type a CAST names.
func pgTypeNamed(name string) (string, bool) {
	n := strings.Join(strings.Fields(strings.ToLower(name)), " ")
	switch n {
	case "int", "integer", "int4":
		return "int4", true
	case "bigint", "int8":
		return "int8", true
	case "numeric", "decimal":
		return "numeric", true
	case "double precision", "float8", "float":
		return "float8", true
	case "boolean", "bool":
		return "bool", true
	case "text", "varchar", "character varying":
		return "text", true
	case "json":
		return "json", true
	case "jsonb":
		return "jsonb", true
	case "timestamp", "timestamp without time zone":
		return "timestamp", true
	case "timestamptz", "timestamp with time zone":
		return "timestamptz", true
	case "date":
		return "date", true
	case "interval":
		return "interval", true
	case "text[]", "_text":
		return "_text", true
	}
	return "", false
}

// pgExplicitCast is CAST(v AS name): an untyped literal through the type's input function; a value
// of the type itself as it is; otherwise the cast pg_cast lists, or I/O conversion through text.
func pgExplicitCast(env pgEnv, v any, name string) (any, error) {
	t, ok := pgTypeNamed(name)
	if !ok {
		// smallint, real and the rest no stackql value has.
		return nil, pgErrorf("omnisdk does not implement casts to %s", name)
	}
	v = pgNormalize(v)
	from := pgTypeNameOf(v)
	if from == "unknown" || from == "text" {
		return pgInput(env, pgTextOrEmpty(v), t)
	}
	if from == t {
		return v, nil
	}
	if t == "text" {
		s, _ := pgTextOf(v)
		return s, nil
	}
	switch x := v.(type) {
	case pgNumeric:
		switch t {
		case "int4", "int2", "int8":
			r := x.clone()
			r.round(0)
			i, ok := r.toInt64()
			if !ok {
				return nil, pgErrorf("%s out of range", pgRangeName(t))
			}
			return pgIntOf(i, t)
		case "float8", "float4":
			return pgFloat8(x.toFloat64()), nil
		}
	case pgFloat8:
		switch t {
		case "int4", "int2", "int8":
			r := math.RoundToEven(float64(x))
			if r != r || r < -9223372036854775808.0 || r >= 9223372036854775808.0 {
				return nil, pgErrorf("%s out of range", pgRangeName(t))
			}
			return pgIntOf(int64(r), t)
		case "numeric":
			return float8ToNumeric(float64(x))
		}
	case pgInt4, int64:
		i := int64(0)
		if y, ok := x.(pgInt4); ok {
			i = int64(y)
		} else {
			i = x.(int64)
		}
		switch t {
		case "int4", "int2", "int8":
			return pgIntOf(i, t)
		case "numeric":
			return int64ToNumeric(i), nil
		case "float8", "float4":
			return pgFloat8(float64(i)), nil
		case "bool":
			if _, isInt4 := x.(pgInt4); isInt4 {
				return i != 0, nil
			}
		}
	case bool:
		if t == "int4" {
			if x {
				return pgInt4(1), nil
			}
			return pgInt4(0), nil
		}
	case pgJSON:
		if t == "jsonb" {
			return pgJSONBIn(string(x))
		}
	case pgJSONB:
		if t == "json" {
			return pgJSON(string(x)), nil
		}
	case pgDate:
		switch t {
		case "timestamp":
			return pgDateToTimestamp(x)
		case "timestamptz":
			return pgDateToTimestamptz(x)
		}
	case pgTimestamp:
		switch t {
		case "timestamptz":
			return pgTimestampToTimestamptz(x)
		case "date":
			return pgTimestampDate(int64(x), false)
		}
	case pgTimestamptz:
		switch t {
		case "timestamp":
			return pgTimestamptzToTimestamp(x)
		case "date":
			return pgTimestampDate(int64(x), true)
		}
	}
	return nil, pgErrorf("cannot cast type %s to %s", pgDisplayName(from), pgDisplayName(t))
}

func pgRangeName(t string) string {
	switch t {
	case "int2":
		return "smallint"
	case "int8":
		return "bigint"
	}
	return "integer"
}

func pgIntOf(i int64, t string) (any, error) {
	switch t {
	case "int8":
		return i, nil
	case "int2":
		if i < -32768 || i > 32767 {
			return nil, pgErrorf("smallint out of range")
		}
	default:
		if i < -2147483648 || i > 2147483647 {
			return nil, pgErrorf("integer out of range")
		}
	}
	return pgInt4(i), nil
}

// pgCompare orders two non-NULL values of the built-in types as their comparison operators do: an
// untyped literal read as the other operand's type, numbers across integer, numeric and float8
// alike, text by bytes (the C collation).
func pgCompare(env pgEnv, a, b any) (int, error) {
	a, b = pgNormalize(a), pgNormalize(b)
	ta, tb := pgTypeNameOf(a), pgTypeNameOf(b)
	var err error
	if ta == "unknown" && tb != "unknown" {
		if a, err = pgInput(env, pgTextOrEmpty(a), tb); err != nil {
			return 0, err
		}
		ta = tb
	} else if tb == "unknown" && ta != "unknown" {
		if b, err = pgInput(env, pgTextOrEmpty(b), ta); err != nil {
			return 0, err
		}
		tb = ta
	}
	numeric := func(t string) bool { return t == "int4" || t == "int8" || t == "numeric" || t == "float8" }
	switch {
	case numeric(ta) && numeric(tb):
		if ta == "float8" || tb == "float8" {
			f := func(v any, t string) float64 {
				if t == "float8" {
					return float64(v.(pgFloat8))
				}
				c, _ := pgCast(v, t, "float8")
				return float64(c.(pgFloat8))
			}
			return float8Cmp(f(a, ta), f(b, tb)), nil
		}
		na, err := pgNumericOf(a)
		if err != nil {
			return 0, err
		}
		nb, err := pgNumericOf(b)
		if err != nil {
			return 0, err
		}
		return cmpVar(na, nb), nil
	case (ta == "text" || ta == "unknown") && (tb == "text" || tb == "unknown"):
		return strings.Compare(pgTextOrEmpty(a), pgTextOrEmpty(b)), nil
	case ta == "bool" && tb == "bool":
		x, y := a.(bool), b.(bool)
		switch {
		case x == y:
			return 0, nil
		case !x:
			return -1, nil
		}
		return 1, nil
	}
	return 0, pgErrorf("operator does not exist: %s >= %s", pgDisplayName(ta), pgDisplayName(tb))
}

// float8Cmp is float8_cmp_internal: NaN above every other value and equal to itself.
func float8Cmp(a, b float64) int {
	switch {
	case a != a:
		if b != b {
			return 0
		}
		return 1
	case b != b:
		return -1
	case a > b:
		return 1
	case a < b:
		return -1
	}
	return 0
}
