package sqlfn_test

import (
	"database/sql"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/sqlfn"
	_ "modernc.org/sqlite"
)

// The SQLite catalogue is checked against SQLite itself: the build stackql embeds
// (modernc.org/sqlite), so a function behaves here exactly as it does in a stackql query on the SQLite
// backend. Every case is run both ways and must agree in value and in storage class — SQLite tells
// 1 from 1.0 — or must fail both ways.

// pool is the argument domain: each SQLite storage class, with the values that decide edge cases —
// zero, signs, the integer limits, integral and fractional reals, numeric and non-numeric text,
// whitespace, case, non-ASCII, JSON, and a blob.
var pool = []any{
	nil,
	int64(0), int64(1), int64(-1), int64(42), int64(-7), int64(math.MaxInt64), int64(math.MinInt64),
	0.0, 1.5, -2.5, 0.1, 3.0, -0.0, 1e20, 2.5e-7,
	"", "a", "abc", "ABC", "  pad  ", "12", "12.5", "1e3", "-3", "12abc", "0x1A", "héllo", "%_", "a.b",
	`{"a":1,"b":[1,2,{"c":"x"}],"d":null}`, `[1,"x",null,2.5,true]`, `"str"`, `{bad`,
	[]byte{0x00, 0x41, 0xff},
}

// small is a reduced domain for functions of three or more arguments.
var small = []any{nil, int64(2), int64(-1), 1.5, "", "abc", "12", `{"a":1}`}

type parity struct {
	name  string
	sql   string  // the call, with ? for each argument; default name(?, ?, …)
	args  [][]any // explicit cases; when nil, every tuple of arity from the domain
	arity []int   // arities to enumerate from the domain
	dom   []any   // domain to enumerate; default pool, small for three or more
}

func tuples(dom []any, n int) [][]any {
	out := [][]any{{}}
	for i := 0; i < n; i++ {
		var next [][]any
		for _, t := range out {
			for _, v := range dom {
				c := append(append([]any{}, t...), v)
				next = append(next, c)
			}
		}
		out = next
	}
	return out
}

func (p parity) cases() [][]any {
	out := append([][]any{}, p.args...)
	for _, n := range p.arity {
		dom := p.dom
		if dom == nil {
			dom = pool
			if n >= 3 {
				dom = small
			}
		}
		out = append(out, tuples(dom, n)...)
	}
	return out
}

func (p parity) query(n int) string {
	if p.sql != "" {
		return "select " + p.sql
	}
	qs := make([]string, n)
	for i := range qs {
		qs[i] = "?"
	}
	return "select " + p.name + "(" + strings.Join(qs, ", ") + ")"
}

// canonical is a value as SQLite stores it: NULL, INTEGER, REAL, TEXT or BLOB. A Go bool is SQLite's
// integer 1 or 0; SQLite has no boolean.
func canonical(v any) (string, any) {
	switch x := v.(type) {
	case nil:
		return "null", nil
	case bool:
		if x {
			return "integer", int64(1)
		}
		return "integer", int64(0)
	case int:
		return "integer", int64(x)
	case int64:
		return "integer", x
	case float64:
		return "real", x
	case string:
		return "text", x
	case []byte:
		return "blob", string(x)
	}
	// TEXT carrying SQLite's JSON subtype is still TEXT.
	if r := reflect.ValueOf(v); r.Kind() == reflect.String {
		return "text", r.String()
	}
	return fmt.Sprintf("%T", v), v
}

func same(a, b any) bool {
	ta, va := canonical(a)
	tb, vb := canonical(b)
	if ta != tb {
		return false
	}
	if fa, ok := va.(float64); ok {
		fb := vb.(float64)
		return fa == fb || (math.IsNaN(fa) && math.IsNaN(fb))
	}
	return va == vb
}

func show(v any) string {
	t, c := canonical(v)
	if s, ok := c.(string); ok {
		return fmt.Sprintf("%s %q", t, s)
	}
	return fmt.Sprintf("%s %v", t, c)
}

// checkParityLiteral is checkParity for calls whose SQL writes some arguments as literals: only the
// arguments the SQL binds with ? are bound, in order, and every argument is passed to the function.
func checkParityLiteral(t *testing.T, cat sqlfn.Catalog, specs []parity) {
	t.Helper()
	checkParityBound(t, cat, specs, true)
}

func checkParity(t *testing.T, cat sqlfn.Catalog, specs []parity) {
	t.Helper()
	checkParityBound(t, cat, specs, false)
}

func checkParityBound(t *testing.T, cat sqlfn.Catalog, specs []parity, literal bool) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	failures := map[string][]string{}
	counts := map[string]int{}
	for _, p := range specs {
		fn, ok := cat.Get(p.name)
		if !ok {
			failures[p.name] = append(failures[p.name], "missing from the catalogue")
			continue
		}
		for _, args := range p.cases() {
			counts[p.name]++
			var want any
			bound := args
			if literal {
				bound = args[:strings.Count(p.sql, "?")]
			}
			werr := db.QueryRow(p.query(len(args)), bound...).Scan(&want)
			got, gerr := func() (v any, err error) {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("panic: %v", r)
					}
				}()
				return fn.Call(args)
			}()
			switch {
			case werr != nil && gerr != nil:
			case werr != nil:
				failures[p.name] = append(failures[p.name], fmt.Sprintf("%s(%s): sqlite errors (%v), got %s", p.name, showArgs(args), werr, show(got)))
			case gerr != nil:
				failures[p.name] = append(failures[p.name], fmt.Sprintf("%s(%s): want %s, got error %v", p.name, showArgs(args), show(want), gerr))
			case !same(want, got):
				failures[p.name] = append(failures[p.name], fmt.Sprintf("%s(%s): want %s, got %s", p.name, showArgs(args), show(want), show(got)))
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
		t.Errorf("%s: %d of %d cases differ from SQLite:\n  %s", n, len(f), counts[n], strings.Join(shown, "\n  "))
	}
}

func showArgs(args []any) string {
	s := make([]string, len(args))
	for i, a := range args {
		s[i] = show(a)
	}
	return strings.Join(s, ", ")
}
