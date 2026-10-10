package sqlfn_test

import (
	"math"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
)

// The operators a plan calls as functions are checked against each dialect's own operator syntax.

func TestSQLiteParityOperators(t *testing.T) {
	specs := []parity{
		{name: "||", sql: "? || ?", arity: []int{2}},
		{name: "||", sql: "? || ? || ?", arity: []int{3}},
		{name: "is_null", sql: "? IS NULL", arity: []int{1}},
		{name: "between", sql: "? BETWEEN ? AND ?", arity: []int{3}},
	}
	checkParity(t, sqliteCatalog(t), specs)
	var casts []parity
	for _, typ := range []string{"INTEGER", "INT", "BIGINT", "TEXT", "VARCHAR(10)", "CLOB", "BLOB", "REAL",
		"FLOAT", "DOUBLE PRECISION", "NUMERIC", "DECIMAL(5,2)", "BOOLEAN", "DATE", "CHARINT", "FLOATING POINT",
		"BLOBINT", "XYZ"} {
		var args [][]any
		for _, v := range pool {
			args = append(args, []any{v, typ})
		}
		for _, v := range []any{"7.0", "7", " 9 ", "1e3", "-0", "9223372036854775807", "9223372036854775808",
			"1.5e2x", ".", "  .5", "12abc", "0x10", 1e300, 7.0, 4503599627370496.0, "4503599627370497.0"} {
			args = append(args, []any{v, typ})
		}
		casts = append(casts, parity{name: "cast", sql: "CAST(? AS " + typ + ")", args: args})
	}
	checkParityLiteral(t, sqliteCatalog(t), casts)
}

func TestPgParityOperators(t *testing.T) {
	db := postgres(t)
	cat := pgCatalog(t)
	vals := []any{nil, "", "a", "b", "10", int64(9), int64(10), sqlfn.PgInt4(3), 2.5, sqlfn.PgFloat8(10), true, false,
		sqlfn.PgUnknown("9"), sqlfn.PgUnknown("x"), sqlfn.PgJSON(`{"a":1}`), sqlfn.PgFloat8(math.NaN())}
	var cases []pgOpCase
	for _, a := range vals {
		cases = append(cases, pgOpCase{"is_null", []any{a}, "(%s) IS NULL"})
		for _, b := range vals {
			cases = append(cases, pgOpCase{"||", []any{a, b}, "%s || %s"})
			for _, c := range []any{nil, int64(5), sqlfn.PgUnknown("z"), "c", 20.5} {
				cases = append(cases, pgOpCase{"between", []any{a, b, c}, "%s BETWEEN %s AND %s"})
			}
		}
	}
	for _, typ := range []string{"integer", "bigint", "numeric", "double precision", "boolean",
		"text", "json", "jsonb", "text[]"} {
		for _, v := range append(vals, 2.5, -2.5, 3.5, 1e20, sqlfn.PgFloat8(2.5), sqlfn.PgFloat8(3.5), sqlfn.PgFloat8(1e19),
			sqlfn.PgUnknown("{a,b}"), sqlfn.PgUnknown("[1, 2]"), int64(70000), sqlfn.PgInt4(0), sqlfn.PgUnknown(" 12 ")) {
			cases = append(cases, pgOpCase{"cast", []any{v, sqlfn.PgUnknown(typ)}, "CAST(%s AS " + typ + ")"})
		}
	}
	checkPgOps(t, db, cat, cases)
}

func TestCatalogRefusesDuplicates(t *testing.T) {
	c := sqliteCatalog(t)
	f, _ := c.Get("lower")
	if _, err := sqlfn.With(c, f); err == nil {
		t.Error("accepted a second lower")
	}
}
