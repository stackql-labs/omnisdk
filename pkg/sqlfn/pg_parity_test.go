package sqlfn_test

import (
	"database/sql"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
)

// The Postgres catalogue is checked against Postgres. Each argument is written into the SQL as
// stackql's data reaches Postgres: a provider string as text, a provider integer as bigint, a
// provider number as numeric, a boolean as boolean, a query's string literal untyped, and so on.
// A result must agree in type (pg_typeof) and in text (::text), or both sides must fail.

// pgLiteral is v written as SQL Postgres reads with v's type.
func pgLiteral(v any) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	switch x := v.(type) {
	case nil:
		return "NULL"
	case sqlfn.PgUnknown:
		return quote(string(x))
	case string:
		return quote(x) + "::text"
	case int64:
		return "(" + strconv.FormatInt(x, 10) + ")::bigint"
	case sqlfn.PgInt4:
		return "(" + strconv.FormatInt(int64(x), 10) + ")::integer"
	case float64:
		return quote(strconv.FormatFloat(x, 'f', -1, 64)) + "::numeric"
	case sqlfn.PgFloat8:
		return quote(strconv.FormatFloat(float64(x), 'g', -1, 64)) + "::float8"
	case bool:
		return strconv.FormatBool(x)
	case sqlfn.PgJSON:
		return quote(string(x)) + "::json"
	case sqlfn.PgJSONB:
		return quote(string(x)) + "::jsonb"
	case sqlfn.PgTextArr:
		return quote(sqlfn.PgArrayText(x)) + "::text[]"
	}
	if typ, text, ok := sqlfn.PgText(v); ok {
		return quote(text) + "::" + typ
	}
	panic(fmt.Sprintf("no literal for %T", v))
}

// pgCall is the SQL of name(args), as stackql sends it: the first argument of
// json_extract_path_text (given a path) and of json_array_elements_text cast to json.
func pgCallSQL(name string, args []any) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = pgLiteral(a)
		if i == 0 && (name == "json_extract_path_text" && len(args) > 1 || name == "json_array_elements_text") {
			parts[i] = "(" + parts[i] + ")::json"
		}
	}
	if name == "position" {
		// position(a, b) is a syntax error: the grammar takes position(b IN a) only.
		name = "pg_catalog.position"
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

type pgCase struct {
	name string
	args []any
}

// pgCatalog is the Postgres catalogue for the oracle's architecture, as its version reports it.
func pgCatalog(t *testing.T) sqlfn.Catalog {
	t.Helper()
	var version string
	if err := postgres(t).QueryRow("select version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	arch := sqlfn.PostgresAMD64
	if strings.Contains(version, "aarch64") {
		arch = sqlfn.PostgresARM64
	}
	c, err := sqlfn.BuiltinsFor(sqlfn.Postgres, sqlfn.WithPostgresArch(arch))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// pgOpCase is a case whose SQL is an operator's: sql is a format with a %s per argument.
type pgOpCase struct {
	name string
	args []any
	sql  string
}

// checkPg runs each case through Postgres and the catalogue.
func checkPg(t *testing.T, cases []pgCase) {
	t.Helper()
	ops := make([]pgOpCase, len(cases))
	for i, c := range cases {
		ops[i] = pgOpCase{c.name, c.args, ""}
	}
	checkPgOps(t, postgres(t), pgCatalog(t), ops)
}

// checkPgOps is checkPg over cases written as their own SQL.
func checkPgOps(t *testing.T, db *sql.DB, cat sqlfn.Catalog, cases []pgOpCase) {
	t.Helper()
	failures := map[string][]string{}
	counts := map[string]int{}
	for _, c := range cases {
		counts[c.name]++
		fn, ok := cat.Get(c.name)
		if !ok {
			failures[c.name] = append(failures[c.name], "missing from the catalogue")
			continue
		}
		expr := pgCallSQL(c.name, c.args)
		if c.sql != "" {
			lits := make([]any, 0, len(c.args))
			for _, a := range c.args {
				lits = append(lits, pgLiteral(a))
			}
			expr = fmt.Sprintf(c.sql, lits[:strings.Count(c.sql, "%s")]...)
		}
		got, gerr := func() (v any, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			return fn.Call(c.args)
		}()
		if fn.Shape() == sqlfn.Table {
			want, werr := pgRows(db, "select * from "+expr)
			var gotRows [][]string
			if gerr == nil {
				for _, r := range got.([]map[string]any) {
					var row []string
					for _, col := range fn.Columns() {
						row = append(row, pgShow(r[col]))
					}
					gotRows = append(gotRows, row)
				}
			}
			if (werr != nil) != (gerr != nil) || (werr == nil && fmt.Sprint(want) != fmt.Sprint(gotRows)) {
				failures[c.name] = append(failures[c.name], fmt.Sprintf("%s: want %v (%v), got %v (%v)", expr, want, werr, gotRows, gerr))
			}
			continue
		}
		var wantText sql.NullString
		var wantType string
		werr := db.QueryRow(fmt.Sprintf("select (%s)::text, pg_typeof(%s)::text", expr, expr)).Scan(&wantText, &wantType)
		switch {
		case werr != nil && gerr != nil:
		case werr != nil:
			failures[c.name] = append(failures[c.name], fmt.Sprintf("%s: postgres errors (%v), got %s", expr, werr, pgShow(got)))
		case gerr != nil:
			failures[c.name] = append(failures[c.name], fmt.Sprintf("%s: want %s %q, got error %v", expr, wantType, wantText.String, gerr))
		default:
			gotType, gotText, notNull := sqlfn.PgText(got)
			if got == nil {
				notNull = false
			}
			if wantText.Valid != notNull || (notNull && gotText != wantText.String) || (got != nil && gotType != wantType) {
				failures[c.name] = append(failures[c.name], fmt.Sprintf("%s: want %s %s, got %s %s", expr, wantType, nullable(wantText), gotType, pgShow(got)))
			}
		}
	}
	names := make([]string, 0, len(failures))
	for n := range failures {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := failures[n]
		shown := f
		if len(shown) > 6 {
			shown = shown[:6]
		}
		t.Errorf("%s: %d of %d cases differ from Postgres:\n  %s", n, len(f), counts[n], strings.Join(shown, "\n  "))
	}
}

func nullable(s sql.NullString) string {
	if !s.Valid {
		return "NULL"
	}
	return strconv.Quote(s.String)
}

func pgShow(v any) string {
	if v == nil {
		return "NULL"
	}
	_, s, _ := sqlfn.PgText(v)
	return strconv.Quote(s)
}

func pgRows(db *sql.DB, q string) ([][]string, error) {
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = nullable(v)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// pgNumbers are numeric-typed argument values across the cases that decide numeric behaviour.
var pgNumbers = []any{
	nil, int64(0), int64(1), int64(-1), int64(42), int64(math.MaxInt64), int64(math.MinInt64),
	sqlfn.PgInt4(0), sqlfn.PgInt4(7), sqlfn.PgInt4(-3), sqlfn.PgInt4(math.MinInt32),
	0.0, 1.5, -2.5, 0.1, 2.675, 1e20, 1.23456789012345678e-7, 123456.789, 0.5, -0.5, 9.99,
	sqlfn.PgFloat8(0.5), sqlfn.PgFloat8(-2.5), sqlfn.PgFloat8(1e300), sqlfn.PgFloat8(math.Inf(1)),
	sqlfn.PgFloat8(math.NaN()), sqlfn.PgFloat8(1e-310), sqlfn.PgFloat8(2),
	sqlfn.PgUnknown("2.5"), sqlfn.PgUnknown("abc"), sqlfn.PgUnknown("-7"), sqlfn.PgUnknown("1e3"),
	sqlfn.PgUnknown("NaN"), sqlfn.PgUnknown(" 12 "), "12", true,
}

func TestPgParityMath(t *testing.T) {
	postgresImage(t)
	var cases []pgCase
	for _, n := range []string{"abs", "ceil", "ceiling", "floor", "round", "trunc", "sign", "sqrt", "cbrt",
		"exp", "ln", "log", "log10", "degrees", "radians", "sin", "cos", "tan", "asin", "acos", "atan",
		"sinh", "cosh", "tanh", "asinh", "acosh", "atanh", "scale", "min_scale", "trim_scale", "factorial"} {
		for _, v := range pgNumbers {
			cases = append(cases, pgCase{n, []any{v}})
		}
	}
	two := []any{nil, int64(3), int64(-2), sqlfn.PgInt4(2), sqlfn.PgInt4(0), 1.5, -0.5, 0.0, 2.0, 10.0,
		sqlfn.PgFloat8(0.5), sqlfn.PgFloat8(-3), sqlfn.PgUnknown("2"), sqlfn.PgUnknown("1.25"), int64(1000)}
	for _, n := range []string{"round", "trunc", "power", "pow", "mod", "div", "log", "atan2", "gcd", "lcm"} {
		for _, a := range two {
			for _, b := range two {
				cases = append(cases, pgCase{n, []any{a, b}})
			}
		}
	}
	cases = append(cases, pgCase{"pi", nil})
	for _, digits := range []int64{-5, -1, 0, 1, 2, 3, 10, 30} {
		for _, v := range []any{2.675, 1.5, -2.5, 123456.789, 0.0005, 99999.99999, 1e20} {
			cases = append(cases, pgCase{"round", []any{v, sqlfn.PgInt4(digits)}}, pgCase{"trunc", []any{v, sqlfn.PgInt4(digits)}})
		}
	}
	checkPg(t, cases)
}

// TestPgParityFloat8Sweep runs the float8 functions over arguments spread across every range their
// algorithms split on, out to the large reductions.
func TestPgParityFloat8Sweep(t *testing.T) {
	postgresImage(t)
	var args []any
	x := uint64(0x9E3779B97F4A7C15)
	for i := 0; i < 400; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		mag := math.Pow(10, float64(int(x%40))-20) * (1 + float64(x>>11)/float64(1<<53))
		if x&1 == 1 {
			mag = -mag
		}
		args = append(args, sqlfn.PgFloat8(mag))
	}
	for _, v := range []float64{1e8, 1.05e8, 2e9, 1e15, 1e22, 1.7e308, math.Pi, math.Pi / 2, 0.855, 2.4263, 0.126} {
		args = append(args, sqlfn.PgFloat8(v), sqlfn.PgFloat8(-v))
	}
	var cases []pgCase
	for _, n := range []string{"sin", "cos", "tan", "asin", "acos", "atan", "cbrt", "exp", "ln", "log10",
		"sinh", "cosh", "tanh", "asinh", "acosh", "atanh", "sqrt"} {
		for _, v := range args {
			cases = append(cases, pgCase{n, []any{v}})
		}
	}
	for i := 0; i+1 < len(args); i += 2 {
		cases = append(cases, pgCase{"atan2", []any{args[i], args[i+1]}}, pgCase{"power", []any{args[i], args[i+1]}})
	}
	checkPg(t, cases)
}

// pgStrings are text-typed argument values: ASCII, multibyte, quotes, backslashes, separators.
var pgStrings = []any{
	nil, "", "a", "abc", "Hello World", "héllo wörld", "日本語テキスト", "  padded  ", "xxhixx", "a,b,,c",
	"it's", `back\slash`, "MiXeD cAsE_123", "select", "user_name", "Ünïcödé", "a.b.c", "abcabcabc", "😀x😀",
	sqlfn.PgUnknown("lit"), sqlfn.PgUnknown(""), int64(42), true, 1.5,
}

func TestPgParityText(t *testing.T) {
	var cases []pgCase
	for _, n := range []string{"lower", "upper", "initcap", "length", "char_length", "character_length",
		"octet_length", "bit_length", "reverse", "ascii", "md5", "quote_ident", "quote_literal",
		"quote_nullable", "btrim", "ltrim", "rtrim", "concat"} {
		for _, v := range pgStrings {
			cases = append(cases, pgCase{n, []any{v}})
		}
	}
	seconds := []any{nil, "", "a", "x", "b", ",", "l", "abc", " ", "ö", "😀", sqlfn.PgUnknown("c")}
	for _, n := range []string{"strpos", "position", "btrim", "ltrim", "rtrim", "starts_with", "string_to_array",
		"concat", "concat_ws"} {
		for _, v := range pgStrings {
			for _, s := range seconds {
				cases = append(cases, pgCase{n, []any{v, s}})
			}
		}
	}
	ints := []any{nil, sqlfn.PgInt4(-3), sqlfn.PgInt4(-1), sqlfn.PgInt4(0), sqlfn.PgInt4(1), sqlfn.PgInt4(2),
		sqlfn.PgInt4(5), sqlfn.PgInt4(40), sqlfn.PgInt4(math.MaxInt32), sqlfn.PgInt4(math.MinInt32), int64(3),
		sqlfn.PgUnknown("2"), sqlfn.PgUnknown("x")}
	for _, n := range []string{"substr", "substring", "left", "right", "repeat", "lpad", "rpad"} {
		for _, v := range pgStrings {
			for _, i := range ints {
				cases = append(cases, pgCase{n, []any{v, i}})
			}
		}
	}
	for _, v := range pgStrings {
		for _, i := range ints {
			for _, j := range []any{nil, sqlfn.PgInt4(-1), sqlfn.PgInt4(0), sqlfn.PgInt4(2), sqlfn.PgInt4(100), sqlfn.PgInt4(math.MaxInt32)} {
				cases = append(cases, pgCase{"substr", []any{v, i, j}}, pgCase{"substring", []any{v, i, j}})
			}
			for _, f := range []any{nil, "", "xy", "ö😀"} {
				cases = append(cases, pgCase{"lpad", []any{v, i, f}}, pgCase{"rpad", []any{v, i, f}})
			}
		}
		for _, sep := range []any{nil, "", ",", "b", "abc", "."} {
			for _, i := range []any{nil, sqlfn.PgInt4(-4), sqlfn.PgInt4(-2), sqlfn.PgInt4(-1), sqlfn.PgInt4(0), sqlfn.PgInt4(1), sqlfn.PgInt4(2), sqlfn.PgInt4(3), sqlfn.PgInt4(9)} {
				cases = append(cases, pgCase{"split_part", []any{v, sep, i}})
			}
			for _, ns := range []any{nil, "", "b", "a"} {
				cases = append(cases, pgCase{"string_to_array", []any{v, sep, ns}}, pgCase{"string_to_table", []any{v, sep, ns}})
			}
			cases = append(cases, pgCase{"string_to_table", []any{v, sep}})
		}
		for _, from := range []any{"", "l", "lo", "abc", "öé"} {
			for _, to := range []any{"", "L", "LO", "xyz1", "😀"} {
				cases = append(cases, pgCase{"translate", []any{v, from, to}}, pgCase{"replace", []any{v, from, to}})
			}
		}
		cases = append(cases, pgCase{"concat", []any{v, int64(1), nil, true, 2.5, sqlfn.PgFloat8(0.1)}},
			pgCase{"concat_ws", []any{"-", v, nil, false, sqlfn.PgJSON(`{"a": 1}`)}})
	}
	for _, c := range []any{nil, sqlfn.PgInt4(-1), sqlfn.PgInt4(0), sqlfn.PgInt4(65), sqlfn.PgInt4(127), sqlfn.PgInt4(128),
		sqlfn.PgInt4(0x7ff), sqlfn.PgInt4(0x800), sqlfn.PgInt4(0xd800), sqlfn.PgInt4(0xdfff), sqlfn.PgInt4(0xffff),
		sqlfn.PgInt4(0x10000), sqlfn.PgInt4(0x10ffff), sqlfn.PgInt4(0x110000), int64(66), sqlfn.PgUnknown("67")} {
		cases = append(cases, pgCase{"chr", []any{c}})
	}
	for _, v := range []any{nil, sqlfn.PgInt4(0), sqlfn.PgInt4(255), sqlfn.PgInt4(-1), sqlfn.PgInt4(math.MinInt32), int64(-1),
		int64(math.MaxInt64), int64(math.MinInt64), sqlfn.PgUnknown("16"), 1.5} {
		cases = append(cases, pgCase{"to_hex", []any{v}})
	}
	for _, v := range []any{nil, int64(5), true, 1.5, sqlfn.PgJSON(`{"a":1}`), sqlfn.PgUnknown("x")} {
		cases = append(cases, pgCase{"quote_literal", []any{v}}, pgCase{"quote_nullable", []any{v}})
	}
	for _, f := range []string{"", "%", "%%", "%s", "%s %s", "%I", "%L", "%1$s %1$s", "%2$s %1$s", "%0$s", "%-5s|",
		"%5s|", "%*s|", "%-*s|", "%*1$s|", "%3$*1$s|", "%*0$s", "%10L", "%x", "%1", "abc %s def", "%s%", "%3$s",
		"%99999999999s", "%-10I|", "%é"} {
		for _, args := range [][]any{nil, {"a"}, {"x y", int64(3)}, {sqlfn.PgInt4(4), "ab", "cd"}, {nil, nil},
			{true, 1.5, sqlfn.PgJSON(`[1]`)}, {"select", `it's\`}, {sqlfn.PgInt4(-6), "z"}, {"7", "q"}} {
			cases = append(cases, pgCase{"format", append([]any{f}, args...)})
		}
	}
	checkPg(t, cases)
}

func pgTextArray(elems ...any) sqlfn.PgTextArr {
	out := sqlfn.PgTextArr{}
	for _, e := range elems {
		if e == nil {
			out = append(out, nil)
			continue
		}
		s := e.(string)
		out = append(out, &s)
	}
	return out
}

func TestPgParitySets(t *testing.T) {
	var cases []pgCase
	ints := []any{nil, sqlfn.PgInt4(-3), sqlfn.PgInt4(0), sqlfn.PgInt4(1), sqlfn.PgInt4(5), sqlfn.PgInt4(math.MaxInt32 - 1),
		sqlfn.PgInt4(math.MaxInt32), sqlfn.PgInt4(math.MinInt32), sqlfn.PgUnknown("2")}
	for _, a := range ints {
		for _, b := range ints {
			// Each series is kept short: an ascending one is stepped by at least its span.
			x, y := int64(2), int64(2)
			if v, ok := a.(sqlfn.PgInt4); ok {
				x = int64(v)
			}
			if v, ok := b.(sqlfn.PgInt4); ok {
				y = int64(v)
			}
			if y-x <= 50 {
				cases = append(cases, pgCase{"generate_series", []any{a, b}})
			}
			for _, s := range []any{nil, sqlfn.PgInt4(0), sqlfn.PgInt4(1), sqlfn.PgInt4(2), sqlfn.PgInt4(-2), sqlfn.PgInt4(math.MaxInt32), sqlfn.PgInt4(math.MinInt32)} {
				if st, ok := s.(sqlfn.PgInt4); ok && st != 0 {
					if n := (y - x) / int64(st); n > 50 {
						continue
					}
				}
				cases = append(cases, pgCase{"generate_series", []any{a, b, s}})
			}
		}
	}
	bigs := []any{int64(math.MaxInt64 - 2), int64(math.MaxInt64), int64(math.MinInt64), int64(math.MinInt64 + 1), int64(0), int64(3)}
	for _, a := range bigs {
		for _, b := range bigs {
			for _, s := range []any{int64(1), int64(-1), int64(math.MaxInt64), int64(math.MinInt64)} {
				x, y, st := a.(int64), b.(int64), s.(int64)
				if span := new(big.Int).Sub(big.NewInt(y), big.NewInt(x)); span.Quo(span, big.NewInt(st)).Cmp(big.NewInt(50)) > 0 {
					continue
				}
				cases = append(cases, pgCase{"generate_series", []any{a, b, s}})
			}
		}
	}
	nums := []any{nil, 0.0, 1.5, -2.5, 10.0, sqlfn.PgUnknown("NaN"), sqlfn.PgUnknown("Infinity"), sqlfn.PgUnknown("0.25")}
	for _, a := range nums {
		for _, b := range nums {
			cases = append(cases, pgCase{"generate_series", []any{a, b}})
			for _, s := range []any{0.5, -0.75, 0.0, sqlfn.PgUnknown("NaN"), sqlfn.PgUnknown("-Infinity"), 2.0} {
				cases = append(cases, pgCase{"generate_series", []any{a, b, s}})
			}
		}
	}
	arrays := []any{nil, pgTextArray(), pgTextArray("a"), pgTextArray("a", nil, "c d", ""), pgTextArray("x", "y", "z")}
	for _, a := range arrays {
		cases = append(cases, pgCase{"unnest", []any{a}})
		for _, d := range []any{nil, sqlfn.PgInt4(0), sqlfn.PgInt4(1), sqlfn.PgInt4(2)} {
			cases = append(cases, pgCase{"generate_subscripts", []any{a, d}})
			for _, r := range []any{nil, true, false} {
				cases = append(cases, pgCase{"generate_subscripts", []any{a, d, r}})
			}
		}
		for _, sep := range []any{nil, ",", ""} {
			cases = append(cases, pgCase{"array_to_string", []any{a, sep}})
			for _, ns := range []any{nil, "*", ""} {
				cases = append(cases, pgCase{"array_to_string", []any{a, sep, ns}})
			}
		}
	}
	checkPg(t, cases)
}

func TestPgParityJSONModify(t *testing.T) {
	docs := []any{nil, sqlfn.PgJSONB(`{}`), sqlfn.PgJSONB(`[]`), sqlfn.PgJSONB(`5`), sqlfn.PgJSONB(`"s"`),
		sqlfn.PgJSONB(`{"a": 1, "b": {"c": [1, 2, {"d": null}]}, "n": null}`), sqlfn.PgJSONB(`[1, [2, 3], {"x": null}, null]`),
		sqlfn.PgJSONB(`{"aa": {"b": 2}, "a": [10, 20, 30]}`)}
	paths := []any{nil, pgTextArray(), pgTextArray("a"), pgTextArray("z"), pgTextArray("b", "c", "1"), pgTextArray("b", "c", "-1"),
		pgTextArray("b", "c", "9"), pgTextArray("b", "c", "-9"), pgTextArray("0"), pgTextArray("-1"), pgTextArray("x"),
		pgTextArray("1", "0"), pgTextArray("a", "1"), pgTextArray("a", nil), pgTextArray(nil), pgTextArray(" 1"),
		pgTextArray("+2"), pgTextArray("1 "), pgTextArray("99999999999"), pgTextArray("-2147483648"), pgTextArray("aa", "b"),
		pgTextArray("b", "c", "2", "d"), pgTextArray("b", "new", "deep"), pgTextArray("n", "x")}
	vals := []any{sqlfn.PgJSONB(`"v"`), sqlfn.PgJSONB(`{"k": [true]}`), sqlfn.PgJSONB(`null`), nil}
	var cases []pgCase
	for _, d := range docs {
		cases = append(cases, pgCase{"jsonb_pretty", []any{d}}, pgCase{"jsonb_strip_nulls", []any{d}})
		for _, p := range paths {
			for _, v := range vals {
				cases = append(cases, pgCase{"jsonb_set", []any{d, p, v}}, pgCase{"jsonb_insert", []any{d, p, v}})
				for _, b := range []any{nil, true, false} {
					cases = append(cases, pgCase{"jsonb_set", []any{d, p, v, b}}, pgCase{"jsonb_insert", []any{d, p, v, b}})
				}
			}
		}
	}
	for _, j := range []any{nil, sqlfn.PgJSON(`{"a":null,"b" : [null, {"c":null,"d":1}] , "e":"xA\n"}`), sqlfn.PgJSON(`null`),
		sqlfn.PgJSON(`[null]`), sqlfn.PgJSON(` 1.50 `), sqlfn.PgJSON(`{"a":null}`), sqlfn.PgJSON(`{"a":{"b":null},"c":2}`), "{\"t\":null}"} {
		cases = append(cases, pgCase{"json_strip_nulls", []any{j}})
	}
	arrs := []any{nil, pgTextArray(), pgTextArray("a"), pgTextArray("a", "1"), pgTextArray("a", nil, "b", "x\"y"),
		pgTextArray(nil, "v"), pgTextArray("k1", "v1", "k2", "v2", "k1", "v3")}
	for _, a := range arrs {
		cases = append(cases, pgCase{"json_object", []any{a}})
		for _, b := range arrs {
			cases = append(cases, pgCase{"json_object", []any{a, b}})
		}
	}
	checkPg(t, cases)
}

// pgDateTexts are date and time input: ISO, SQL and textual forms, zones by offset, abbreviation
// and name, Julian days, BC, specials, and malformed and out-of-range text.
var pgDateTexts = []string{
	"2024-02-29", "2023-02-29", "2024-02-29 13:45:01.123456", "2024-02-29T13:45:01Z", "2024-02-29 13:45:01+05:30",
	"2024-02-29 13:45:01-0800", "2024-02-29 13:45:01 PST", "2024-02-29 13:45:01 EDT", "2024-07-01 12:00 America/New_York",
	"2024-01-15 12:00 Europe/London", "2024-03-10 02:30 America/New_York", "2024-11-03 01:30 America/New_York",
	"Feb 29 2024", "29 Feb 2024 1:02pm", "February 29, 2024 01:02:03 AM", "2/29/2024", "02/29/24", "20240229",
	"20240229T134501", "240229", "2024.060", "J2460370", "J2460370.5", "epoch", "infinity", "-infinity",
	"2024-02-29 24:00:00", "2024-02-29 24:00:01", "2024-02-29 12:60:00", "2024-13-01", "0001-01-01 BC",
	"4714-11-24 BC", "4714-11-23 BC", "294276-12-31 23:59:59.999999", "294277-01-01", "1999-12-31 23:59:59.9999995",
	"allballs", "2024-02-29 allballs", "yesterday", "", "garbage", "2024-02-29 13:45:01 XYZ", "2024-02-29 Mars/Olympus",
	"2024-02-29 13:45:01 +16", "Thu, 29 Feb 2024 13:45:01 GMT", "1/2/3", "99-1-1", "10000-01-01", "2024-02-29 13:45:01.5 Europe/Paris",
	"  2024-02-29  ", "2024-02-29 13:45", "13:45", "2024-02-29 1:2:3.4", "Feb-29-2024", "2024-Feb-29", "29-Feb-2024",
}

var pgIntervalTexts = []string{
	"1 day", "1 day 02:03:04", "-1 day +02:03", "1 year 2 mons 3 days 04:05:06.789", "1.5 years", "1.5 months",
	"2 weeks", "3 decades", "1.25 centuries", "1 millennium", "90 minutes", "1.5 hours", "250 ms", "17 us", "@ 1 hour ago",
	"1 day ago", "10:00", "-10:00:00.5", "1-2", "-1-2", "P1Y2M3DT4H5M6S", "PT1.5S", "P1.5Y", "P0001-02-03T04:05:06",
	"P2W", "PT36H", "1 fortnight", "", "1 day 1 day", "2147483647 days", "2147483648 days", "100000000 years",
	"1 second 500 milliseconds", "0", "5", "1.5", "P1Y-2M", "1 hour 30", "1:30:45.123456789",
}

func TestPgParityDates(t *testing.T) {
	var cases []pgCase
	unit := []string{"microseconds", "milliseconds", "second", "minute", "hour", "day", "week", "month", "quarter",
		"year", "decade", "century", "millennium", "epoch", "dow", "isodow", "doy", "julian", "isoyear", "timezone",
		"timezone_hour", "timezone_minute", "DAY", "secs", "nonsense", "us", "mils"}
	for _, s := range pgDateTexts {
		for _, typ := range []string{"timestamptz", "timestamp", "date"} {
			v, err := sqlfn.PgTyped(typ, s)
			if err != nil {
				continue
			}
			for _, u := range unit {
				cases = append(cases, pgCase{"date_part", []any{u, v}}, pgCase{"date_trunc", []any{u, v}})
			}
		}
		// Untyped, the literal is read as Postgres resolves it.
		cases = append(cases, pgCase{"date_trunc", []any{"day", sqlfn.PgUnknown(s)}},
			pgCase{"date_part", []any{"epoch", sqlfn.PgUnknown(s)}},
			pgCase{"date_trunc", []any{"hour", sqlfn.PgUnknown(s), "Asia/Kolkata"}},
			pgCase{"date_trunc", []any{"day", sqlfn.PgUnknown(s), "pst"}})
	}
	for _, s := range pgIntervalTexts {
		v, err := sqlfn.PgTyped("interval", s)
		if err != nil {
			continue
		}
		for _, u := range unit {
			cases = append(cases, pgCase{"date_part", []any{u, v}}, pgCase{"date_trunc", []any{u, v}})
		}
	}
	for _, f := range []any{0.0, 1.5, -1.5, 1e10, -1e12, 1e15, 1e20, 1709214301.123456} {
		cases = append(cases, pgCase{"to_timestamp", []any{sqlfn.PgFloat8(f.(float64))}})
	}
	cases = append(cases, pgCase{"to_timestamp", []any{sqlfn.PgFloat8(math.Inf(1))}}, pgCase{"to_timestamp", []any{sqlfn.PgFloat8(math.NaN())}})
	t.Logf("%d cases", len(cases))
	checkPg(t, cases)
}

// TestPgParityDateInput checks each type's input and output: a cast of each text, compared as text,
// or both failing.
func TestPgParityDateInput(t *testing.T) {
	db := postgres(t)
	cat := pgCatalog(t)
	var cases []pgOpCase
	for _, typ := range []string{"timestamptz", "timestamp", "date"} {
		for _, s := range pgDateTexts {
			if s == "yesterday" {
				continue // the clock is the catalogue's own
			}
			cases = append(cases, pgOpCase{"cast", []any{sqlfn.PgUnknown(s), sqlfn.PgUnknown(typ)}, "CAST(%s AS " + typ + ")"})
		}
	}
	for _, s := range pgIntervalTexts {
		cases = append(cases, pgOpCase{"cast", []any{sqlfn.PgUnknown(s), sqlfn.PgUnknown("interval")}, "CAST(%s AS interval)"})
	}
	checkPgOps(t, db, cat, cases)
}

// TestPgParityNow reads now() and the clock's words inside one Postgres transaction, whose start
// they all are, with the catalogue's clock pinned to it.
func TestPgParityNow(t *testing.T) {
	db := postgres(t)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var epoch float64
	if err := tx.QueryRow("select extract(epoch from now())::float8").Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	sec := math.Floor(epoch)
	at := time.Unix(int64(sec), int64(math.Round((epoch-sec)*1e6))*1000)
	cat, err := sqlfn.BuiltinsFor(sqlfn.Postgres, sqlfn.WithClock(sqlfn.FixedClock(at)))
	if err != nil {
		t.Fatal(err)
	}
	check := func(sqlText string, fn string, args ...any) {
		t.Helper()
		var want string
		if err := tx.QueryRow("select (" + sqlText + ")::text").Scan(&want); err != nil {
			t.Fatal(err)
		}
		f, _ := cat.Get(fn)
		got, err := f.Call(args)
		if err != nil {
			t.Fatalf("%s: %v", sqlText, err)
		}
		if _, text, _ := sqlfn.PgText(got); text != want {
			t.Errorf("%s: want %s, got %s", sqlText, want, text)
		}
	}
	check("now()", "now")
	for _, w := range []string{"now", "today", "yesterday", "tomorrow", "today 10:00", "yesterday allballs"} {
		for _, typ := range []string{"timestamptz", "timestamp", "date"} {
			check("CAST('"+w+"' AS "+typ+")", "cast", sqlfn.PgUnknown(w), sqlfn.PgUnknown(typ))
		}
	}
}

// TestPgParityLike checks like(string, pattern) and like_escape, and that the two together are
// Postgres's LIKE … ESCAPE.
func TestPgParityLike(t *testing.T) {
	values := []any{nil, "", "abc", "ABC", "a%c", "a_c", `a\c`, "héllo", "日本語", "xxhixx", "a!b", "%", "_"}
	patterns := []any{nil, "", "%", "_", "a%", "%c", "a_c", "a\\%c", `a\_c`, `a\\c`, "h_llo", "h%o", "日_語", "%hi%",
		"a!%c", "abc\\", "%%_", "_%_", "ABC", "a", sqlfn.PgUnknown("a%")}
	var cases []pgCase
	for _, v := range values {
		for _, p := range patterns {
			cases = append(cases, pgCase{"like", []any{v, p}})
		}
	}
	for _, p := range patterns {
		for _, e := range []any{nil, "", "!", `\`, "é", "ab", "%"} {
			cases = append(cases, pgCase{"like_escape", []any{p, e}})
		}
	}
	checkPg(t, cases)

	db := postgres(t)
	cat := pgCatalog(t)
	like, _ := cat.Get("like")
	esc, _ := cat.Get("like_escape")
	for _, v := range values[1:] {
		for _, p := range patterns[1:] {
			for _, e := range []string{"", "!", `\`, "é"} {
				var want sql.NullBool
				werr := db.QueryRow(fmt.Sprintf("select %s LIKE %s ESCAPE %s", pgLiteral(v), pgLiteral(p), pgLiteral(e))).Scan(&want)
				rewritten, gerr := esc.Call([]any{p, e})
				var got any
				if gerr == nil {
					got, gerr = like.Call([]any{v, rewritten})
				}
				if (werr != nil) != (gerr != nil) || (werr == nil && got != want.Bool) {
					t.Errorf("%v LIKE %v ESCAPE %q: want %v (%v), got %v (%v)", v, p, e, want.Bool, werr, got, gerr)
				}
			}
		}
	}
}
