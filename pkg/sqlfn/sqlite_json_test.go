package sqlfn_test

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
)

// jsonDocs are JSON inputs: canonical JSON of every type and nesting, SQLite's JSON5 extensions
// (comments, unquoted and single-quoted keys, hex, Infinity and NaN, trailing commas, escapes), and
// malformed text failing at each kind of token.
var jsonDocs = []any{
	nil, "", " ", "null", "true", "false", "0", "-0", "1", "-1", "123456789012345678901234567890",
	"9223372036854775807", "9223372036854775808", "-9223372036854775808", "-9223372036854775809",
	"1.5", "-2.25e-3", "1E400", "-1e400", "0.0", "1.", ".5", "+1", "0x1F", "-0xff", "0x8000000000000000",
	"0x10000000000000000", "Infinity", "-Infinity", "+inf", "NaN", "QNaN", "-nan",
	`""`, `"abc"`, `"a\"b"`, `"\u00e9\n\t"`, `"\ud83d\ude00"`, `"\ud83d"`, `'single'`, `"\x41\v\0"`,
	`"line\
cont"`, "\"tab\there\"", `"\q"`, `"\u12"`,
	"[]", "[1,2,3]", `[1,"x",null,2.5,true,false,[],{}]`, "[[[[1]]]]", "[1,2,]", "[,]", "[1 2]", "[1,,2]",
	"{}", `{"a":1}`, `{"a":1,"b":[1,2,{"c":"x"}],"d":null}`, `{"a":{"b":{"c":{"d":1}}}}`,
	`{"a":1,"a":2}`, `{a:1, $b:2, _c:3}`, `{'k':'v'}`, `{"a":1,}`, `{"a" : 1 , "b" : 2}`, `{"a"1}`,
	`{1:2}`, `{"a":}`, `{"x y":1,"x.y":2,"x[0]":3,"":4,"é":5}`, `{"\u0061":1,"b\"c":2}`,
	"/* c */ [1] // x", "[1] /* open", "\u00a0[1]\u2028", "[1]x", "{", "[", `"unterminated`, "tru",
	"nul", "[true,false,null]", "[1e5,1E-5,-0.0e0]", "01", "-", "--1", "1e", "1e+", ".e1",
	`{"a":[1,{"b":[2,{"c":3}]}],"e":{"f":[]}}`, "[\"\\u0000\"]",
	int64(5), 2.5, []byte(`{"a":1}`), []byte{0x0c}, []byte{0x13, '1'},
}

// jsonPaths are path arguments: every step kind, the end-relative forms, and malformed paths.
var jsonPaths = []any{
	nil, "$", "$.a", "$.b", "$.b[0]", "$.b[2].c", "$.b[#]", "$.b[#-1]", "$.b[#-3]", "$.b[#-9]", "$[0]",
	"$[1]", "$[7]", "$[#-1]", "$[99]", `$."a"`, `$."x y"`, `$."x.y"`, `$."x[0]"`, `$.""`, `$."a`,
	"$.é", `$."\u0061"`, `$."b\"c"`, "$.a.b.c.d", "$.a.b.c", "$.x", "$.x.y", "$[0][0][0][0]",
	"a", "", "$.", "$[", "$[a]", "$[-1]", "$..a", "$ .a", "$[0", "$.a[", "$[#-]", "$[ 0]",
	"$.e.f", "$.e.f[0]", "$.a[1].b[1].c", "$.d", "$[#]", "$.a[#]",
}

// Documents whose payloads sit on each JSONB header-size boundary: 11, 255 and 65535 bytes, and one
// past each.
func init() {
	for _, n := range []int{11, 12, 255, 256, 65535, 65536} {
		jsonDocs = append(jsonDocs, `"`+strings.Repeat("x", n)+`"`,
			"["+strings.TrimSuffix(strings.Repeat("1,", n/2), ",")+"]",
			`{"a":"`+strings.Repeat("y", n-4)+`"}`)
	}
}

func TestSQLiteParityJSONOne(t *testing.T) {
	var specs []parity
	for _, n := range []string{"json", "jsonb", "json_valid", "json_type", "json_array_length",
		"json_error_position", "json_pretty", "json_quote"} {
		specs = append(specs, parity{name: n, arity: []int{1}, dom: append(append([]any{}, jsonDocs...), pool...)})
	}
	var valid [][]any
	for _, d := range jsonDocs {
		for f := int64(0); f <= 16; f++ {
			valid = append(valid, []any{d, f})
		}
	}
	specs = append(specs, parity{name: "json_valid", args: valid})
	var pretty [][]any
	for _, d := range jsonDocs {
		for _, in := range []any{nil, "", "\t", "--", int64(2), "a\x00b"} {
			pretty = append(pretty, []any{d, in})
		}
	}
	specs = append(specs, parity{name: "json_pretty", args: pretty})
	checkParity(t, sqliteCatalog(t), specs)
}

func TestSQLiteParityJSONPath(t *testing.T) {
	var args [][]any
	for _, d := range jsonDocs {
		for _, p := range jsonPaths {
			args = append(args, []any{d, p})
		}
	}
	var specs []parity
	for _, n := range []string{"json_extract", "jsonb_extract", "json_type", "json_array_length",
		"json_remove", "jsonb_remove"} {
		specs = append(specs, parity{name: n, args: args})
	}
	// -> and ->> are operators in SQLite's grammar; abbreviated paths are theirs.
	abbrev := append(append([]any{}, jsonPaths...), "a", "b", int64(0), int64(1), int64(-1), int64(-9),
		"0", "[0]", "[#-1]", "x y", "x.y", 1.5)
	var opArgs [][]any
	for _, d := range jsonDocs {
		for _, p := range abbrev {
			opArgs = append(opArgs, []any{d, p})
		}
	}
	specs = append(specs, parity{name: "->", sql: "? -> ?", args: opArgs}, parity{name: "->>", sql: "? ->> ?", args: opArgs})
	checkParity(t, sqliteCatalog(t), specs)

	var multi [][]any
	for _, d := range jsonDocs {
		multi = append(multi, []any{d, "$.a", "$.b"}, []any{d, "$[0]", "$[9]", "$"}, []any{d, "$.a", "bad"},
			[]any{d, "$.a", nil}, []any{d}, []any{d, "$.d", "$.b[1]", "$.b[2]"})
	}
	checkParity(t, sqliteCatalog(t), []parity{
		{name: "json_extract", args: multi}, {name: "jsonb_extract", args: multi},
		{name: "json_remove", args: multi}, {name: "jsonb_remove", args: multi},
	})
}

// jsonValues are values to insert: every SQL class, and JSON given as JSON (by json()).
var jsonValues = []any{nil, int64(7), -1.5, 1e300, "s", `{"k":[1]}`, ""}

func TestSQLiteParityJSONEdit(t *testing.T) {
	docs := []any{nil, "{}", "[]", `{"a":1,"b":[1,2,{"c":"x"}],"d":null}`, "[1,[2,[3]]]", `{"a":{"b":1}}`,
		"5", `"s"`, "{bad", `{a:1,'b':0x10}`, []byte{0x0c}}
	paths := []any{nil, "$", "$.a", "$.z", "$.a.b", "$.a.b.c", "$.b[0]", "$.b[#]", "$.b[3]", "$.b[9]",
		"$[0]", "$[#]", "$[1][1][0]", "$[1][#]", "$.z[0]", "$.z[#]", "$.q.r[0].s", `$."new key"`,
		`$."e\u0301"`, "$[0].x", "bad", "$[", "$.b[#-1]"}
	var args [][]any
	for _, d := range docs {
		for _, p := range paths {
			for _, v := range jsonValues {
				args = append(args, []any{d, p, v})
			}
		}
		args = append(args, []any{d}, []any{d, "$.a"}, []any{d, "$.a", 1, "$.b", 2},
			[]any{d, "$.a", 1, "$.a.b", 2}, []any{d, "$.a", []byte{1, 2}})
	}
	var specs []parity
	for _, n := range []string{"json_set", "json_insert", "json_replace", "json_array_insert",
		"jsonb_set", "jsonb_insert", "jsonb_replace", "jsonb_array_insert"} {
		specs = append(specs, parity{name: n, args: args})
	}
	checkParity(t, sqliteCatalog(t), specs)
}

func TestSQLiteParityJSONPatch(t *testing.T) {
	docs := []any{nil, "{}", "[]", "1", "null", `{"a":1}`, `{"a":null}`, `{"a":{"b":1,"c":2}}`,
		`{"a":{"b":null}}`, `{"a":[1]}`, `{"x":{"y":{"z":1}}}`, `{"a":1,"b":2,"c":3}`, `{"b":{}}`,
		`{a:1}`, "{bad", `{"a":{"b":{"c":null,"d":4}}}`, `{"\u0061":9}`}
	var args [][]any
	for _, a := range docs {
		for _, b := range docs {
			args = append(args, []any{a, b})
		}
	}
	checkParity(t, sqliteCatalog(t), []parity{{name: "json_patch", args: args}, {name: "jsonb_patch", args: args}})
}

func TestSQLiteParityJSONBuild(t *testing.T) {
	vals := append(append([]any{}, pool...), math.Inf(1), math.Inf(-1))
	var specs []parity
	for _, n := range []string{"json_array", "jsonb_array", "json_quote"} {
		specs = append(specs, parity{name: n, arity: []int{1}, dom: vals})
	}
	specs = append(specs, parity{name: "json_array", arity: []int{0, 2}, dom: small},
		parity{name: "jsonb_array", arity: []int{2}, dom: small},
		parity{name: "json_object", arity: []int{0, 1, 2, 4}, dom: small},
		parity{name: "jsonb_object", arity: []int{2}, dom: small})
	var obj [][]any
	for _, v := range vals {
		obj = append(obj, []any{"k", v}, []any{v, 1}, []any{"a", 1, "a", v})
	}
	specs = append(specs, parity{name: "json_object", args: obj}, parity{name: "jsonb_object", args: obj})
	checkParity(t, sqliteCatalog(t), specs)
}

// TestSQLiteParityJSONB feeds JSONB blobs, as SQLite itself encodes each document, to every function
// that reads JSON, and checks malformed blobs too.
func TestSQLiteParityJSONB(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var blobs []any
	for _, d := range jsonDocs {
		var b any
		if err := db.QueryRow("select jsonb(?)", d).Scan(&b); err == nil && b != nil {
			blobs = append(blobs, b)
		}
	}
	// Truncated and corrupted encodings.
	blobs = append(blobs, []byte{0xcb, 0x02, 0x13, '1'}, []byte{0x2b, 0x13, '1'}, []byte{0x1c, 0x00},
		[]byte{0x3c, 0x17, 'a', 0x00}, []byte{0x23, '1', 'x', '2'}, []byte{0x3b, 0x00, 0x00, 0x00},
		[]byte{0x0d}, []byte{0x5c, 0x17, 'a', 0x23, '1', '2'}, []byte{0xf0, 0, 0, 0, 0, 0, 0, 0, 0},
		[]byte{0x34, '0', 'x', 'g'}, []byte{0x29, '\\', 'q'}, []byte{0x36, '-', '.', '5'}, []byte{0x1a, 0x00})
	var one [][]any
	var path [][]any
	for _, b := range blobs {
		one = append(one, []any{b})
		for _, p := range []any{"$", "$.a", "$.b[2].c", "$[0]", "$[#-1]"} {
			path = append(path, []any{b, p})
		}
	}
	var valid [][]any
	for _, b := range blobs {
		for f := int64(1); f <= 15; f++ {
			valid = append(valid, []any{b, f})
		}
	}
	var specs []parity
	for _, n := range []string{"json", "jsonb", "json_valid", "json_type", "json_array_length",
		"json_error_position", "json_pretty", "json_quote", "json_array", "jsonb_array"} {
		specs = append(specs, parity{name: n, args: one})
	}
	for _, n := range []string{"json_extract", "jsonb_extract", "json_remove", "jsonb_remove", "json_type"} {
		specs = append(specs, parity{name: n, args: path})
	}
	var set [][]any
	for _, b := range blobs {
		set = append(set, []any{b, "$.a", int64(1)}, []any{b, "$[0]", "x"}, []any{"{}", "$.k", b},
			[]any{"[]", "$[#]", b})
	}
	specs = append(specs, parity{name: "json_valid", args: valid}, parity{name: "json_set", args: set},
		parity{name: "jsonb_set", args: set}, parity{name: "json_insert", args: set})
	var patch [][]any
	for _, b := range blobs {
		patch = append(patch, []any{b, `{"a":null,"z":1}`}, []any{`{"a":1}`, b})
	}
	specs = append(specs, parity{name: "json_patch", args: patch}, parity{name: "jsonb_patch", args: patch})
	checkParity(t, sqliteCatalog(t), specs)
}

// TestSQLiteParityJSONEach compares json_each, json_tree and their jsonb twins row for row.
func TestSQLiteParityJSONEach(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := sqliteCatalog(t)
	roots := []any{nil, "$", "$.a", "$.b", "$.b[2]", "$.b[2].c", "$[0]", "$[6]", `$."x y"`, "$.zz", "a", "$[",
		`$."x.y"`, `$.e`, `$.e.f`, "$.a[1]", "$[#-1]"}
	var docs []any
	docs = append(docs, jsonDocs...)
	var blob any
	if err := db.QueryRow("select jsonb(?)", `{"a":[1,{"b":2}],"c":"x"}`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	docs = append(docs, blob)
	cols := "key, value, type, atom, id, parent, fullkey, path, json, root"
	for _, name := range []string{"json_each", "json_tree", "jsonb_each", "jsonb_tree"} {
		fn, ok := cat.Get(name)
		if !ok {
			t.Errorf("%s missing", name)
			continue
		}
		// SELECT * is every column but the hidden ones.
		star, err := db.Query(fmt.Sprintf("select * from %s('[]')", name))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := star.Columns()
		star.Close()
		hidden := map[string]bool{}
		if h, ok := fn.(sqlfn.HiddenColumns); ok {
			for _, c := range h.Hidden() {
				hidden[c] = true
			}
		}
		var visible []string
		for _, c := range fn.Columns() {
			if !hidden[c] {
				visible = append(visible, c)
			}
		}
		if strings.Join(visible, ",") != strings.Join(want, ",") {
			t.Errorf("%s: SELECT * columns %v, want %v", name, visible, want)
		}
		fails := 0
		for _, d := range docs {
			for _, r := range append([]any{"(none)"}, roots...) {
				args := []any{d}
				q := fmt.Sprintf("select %s from %s(?)", cols, name)
				if r != "(none)" {
					args = append(args, r)
					q = fmt.Sprintf("select %s from %s(?, ?)", cols, name)
				}
				want, werr := sqliteRows(db, q, args)
				gotRows, gerr := fn.Call(args)
				var got [][]any
				if gerr == nil {
					for _, row := range gotRows.([]map[string]any) {
						var vals []any
						for _, c := range strings.Split(cols, ", ") {
							vals = append(vals, row[c])
						}
						got = append(got, vals)
					}
				}
				if (werr != nil) != (gerr != nil) || !sameRows(want, got) {
					fails++
					if fails <= 6 {
						t.Errorf("%s(%s): want %v (%v), got %v (%v)", name, showArgs(args), showRows(want), werr, showRows(got), gerr)
					}
				}
			}
		}
		if fails > 6 {
			t.Errorf("%s: %d calls differ", name, fails)
		}
	}
}

func sqliteRows(db *sql.DB, q string, args []any) ([][]any, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

func sameRows(a, b [][]any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		for j := range a[i] {
			if !same(a[i][j], b[i][j]) {
				return false
			}
		}
	}
	return true
}

func showRows(rs [][]any) string {
	var s []string
	for _, r := range rs {
		s = append(s, "("+showArgs(r)+")")
	}
	return strings.Join(s, " ")
}

// node is an expression for nested-xcall parity: SQLite's JSON subtype rides between nested calls.
type node interface{}

type xcall struct {
	name string
	args []node
}

func (c xcall) sql(binds *[]any) string {
	parts := make([]string, len(c.args))
	for i, a := range c.args {
		if sub, ok := a.(xcall); ok {
			parts[i] = sub.sql(binds)
		} else {
			*binds = append(*binds, a)
			parts[i] = "?"
		}
	}
	switch c.name {
	case "->", "->>":
		return "(" + parts[0] + " " + c.name + " " + parts[1] + ")"
	}
	return c.name + "(" + strings.Join(parts, ", ") + ")"
}

func (c xcall) eval(cat sqlfn.Catalog) (any, error) {
	args := make([]any, len(c.args))
	for i, a := range c.args {
		if sub, ok := a.(xcall); ok {
			v, err := sub.eval(cat)
			if err != nil {
				return nil, err
			}
			args[i] = v
		} else {
			args[i] = a
		}
	}
	fn, ok := cat.Get(c.name)
	if !ok {
		return nil, fmt.Errorf("no %s", c.name)
	}
	return fn.Call(args)
}

func TestSQLiteParityJSONSubtype(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := sqliteCatalog(t)
	j := func(s string) xcall { return xcall{"json", []node{s}} }
	exprs := []xcall{
		{"json_array", []node{j("[1]"), "[1]"}},
		{"json_object", []node{"a", j(`{"b":2}`), "c", `{"b":2}`}},
		{"json_set", []node{"{}", "$.a", j("[1,2]")}},
		{"json_set", []node{"{}", "$.a", "[1,2]"}},
		{"json_insert", []node{"[]", "$[#]", xcall{"json_array", []node{int64(1), int64(2)}}}},
		{"json_array", []node{xcall{"json_extract", []node{`{"a":[1]}`, "$.a"}}}},
		{"json_array", []node{xcall{"json_extract", []node{`{"a":"s"}`, "$.a"}}}},
		{"json_array", []node{xcall{"->", []node{`{"a":"s"}`, "$.a"}}}},
		{"json_array", []node{xcall{"->>", []node{`{"a":[1]}`, "$.a"}}}},
		{"json_array", []node{xcall{"json_quote", []node{"x"}}}},
		{"json_array", []node{xcall{"json_pretty", []node{"[1]"}}}},
		{"json_array", []node{xcall{"json_patch", []node{"{}", `{"a":1}`}}}},
		{"json_array", []node{xcall{"json_remove", []node{"[1,2]", "$[0]"}}}},
		{"json_array", []node{xcall{"json_type", []node{"[1]"}}}},
		{"json_array", []node{xcall{"jsonb", []node{"[1]"}}}},
		{"json_array", []node{xcall{"json_extract", []node{`{"a":[1]}`, "$.a", "$.a"}}}},
		{"json_array", []node{xcall{"lower", []node{j("[1]")}}}},
		{"json_array", []node{xcall{"coalesce", []node{nil, j("[1]")}}}},
		{"json_array", []node{xcall{"ifnull", []node{j("[1]"), int64(0)}}}},
		{"json_array", []node{xcall{"iif", []node{int64(1), j("[1]"), int64(0)}}}},
		{"json_array", []node{xcall{"max", []node{j("[1]"), ""}}}},
		{"json_array", []node{xcall{"likely", []node{j("[1]")}}}},
		{"json_array", []node{xcall{"nullif", []node{j("[1]"), "x"}}}},
		{"json_array", []node{xcall{"json_array", []node{xcall{"json_object", []node{"k", j("{}")}}}}}},
		{"json_set", []node{j("{}"), "$.a", xcall{"json_extract", []node{`{"x":{"y":1}}`, "$.x"}}}},
		{"json_set", []node{"[]", "$[0]", xcall{"json_quote", []node{"q"}}}},
		{"json_patch", []node{"{}", xcall{"json_object", []node{"a", j("[1]")}}}},
	}
	for _, e := range exprs {
		var binds []any
		q := "select " + e.sql(&binds)
		var want any
		werr := db.QueryRow(q, binds...).Scan(&want)
		got, gerr := e.eval(cat)
		if (werr != nil) != (gerr != nil) || (werr == nil && !same(want, got)) {
			t.Errorf("%s %v: want %s (%v), got %s (%v)", q, binds, show(want), werr, show(got), gerr)
		}
	}
}
