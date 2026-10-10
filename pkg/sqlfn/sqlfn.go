// Package sqlfn is the catalogue of SQL extension functions omnisdk evaluates: scalar functions,
// and table functions that fan one row out into several.
//
// A function works on row values as they arrive — nil, string, number, bool, JSON object or array —
// and knows nothing of requests, plans or the engine; the engine adapts it. That is what lets a
// caller add its own functions beside the built-ins. Where SQLite and PostgreSQL disagree, the
// behaviour is SQLite's, stackql's default backend: booleans come back as 1 and 0, JSON containers
// as JSON text.
//
// Standard library only.
package sqlfn

import (
	"fmt"
	"sort"
	"time"
)

// Shape is what a function returns.
type Shape int

const (
	// Scalar returns one value.
	Scalar Shape = iota
	// Table returns rows: []map[string]any, one map per row, keyed by Columns.
	Table
)

// Func is one SQL function.
type Func interface {
	Name() string
	// Arity is the number of arguments accepted; max < 0 means any number from min up.
	Arity() (min, max int)
	Shape() Shape
	// Columns names a table function's output columns; nil for a scalar.
	Columns() []string
	// Call evaluates. A scalar returns its value; a table function returns []map[string]any.
	Call(args []any) (any, error)
}

// Catalog is a set of functions by name.
type Catalog interface {
	Get(name string) (Func, bool)
	// Funcs lists every function, ordered by name.
	Funcs() []Func
}

// NewCatalog builds a catalog. A name given twice is an error: which one a query meant would
// depend on order.
func NewCatalog(fns ...Func) (Catalog, error) {
	c := catalog{byName: map[string]Func{}}
	for _, f := range fns {
		if _, dup := c.byName[f.Name()]; dup {
			return nil, fmt.Errorf("sqlfn: %q is defined twice", f.Name())
		}
		c.byName[f.Name()] = f
		c.sorted = append(c.sorted, f)
	}
	sort.Slice(c.sorted, func(i, j int) bool { return c.sorted[i].Name() < c.sorted[j].Name() })
	return c, nil
}

// With is base plus extra. An extra function may not reuse a name base defines.
func With(base Catalog, extra ...Func) (Catalog, error) {
	return NewCatalog(append(append([]Func(nil), base.Funcs()...), extra...)...)
}

type catalog struct {
	byName map[string]Func
	sorted []Func
}

func (c catalog) Get(name string) (Func, bool) { f, ok := c.byName[name]; return f, ok }
func (c catalog) Funcs() []Func                { return append([]Func(nil), c.sorted...) }

// Dialect is the SQL a catalogue speaks: which functions exist and what each does. stackql runs on
// SQLite by default and on Postgres where configured, and the two disagree — in names
// (json_extract against json_extract_path_text) and in semantics (regexp_replace replaces every
// match in stackql's SQLite, the first unless flagged in Postgres).
type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
)

// Option configures a catalogue.
type Option func(*options)

type options struct {
	clock  Clock
	pgArch PostgresArch
}

// PostgresArch is the CPU architecture of the Postgres server: glibc rounds some double
// precision functions (cbrt, log10, the hyperbolic ones) differently on each.
type PostgresArch string

// The architectures stackql's Postgres image is built for.
const (
	PostgresAMD64 PostgresArch = "amd64"
	PostgresARM64 PostgresArch = "arm64"
)

// WithPostgresArch makes the Postgres catalogue's double precision functions round as a server on
// a does. Without it, they round as on amd64.
func WithPostgresArch(a PostgresArch) Option { return func(o *options) { o.pgArch = a } }

// WithClock makes the catalogue's date and time functions read c. Without it, a catalogue reads the
// system time once, at its first read: a catalogue is built for one statement.
func WithClock(c Clock) Option { return func(o *options) { o.clock = c } }

// BuiltinsFor is every function d provides. The empty dialect is SQLite, stackql's default backend.
func BuiltinsFor(d Dialect, opts ...Option) (Catalog, error) {
	o := options{clock: StatementClock(time.Now), pgArch: PostgresAMD64}
	for _, opt := range opts {
		opt(&o)
	}
	switch d {
	case SQLite, "":
		return NewCatalog(sqliteBuiltins(o)...)
	case Postgres:
		if o.pgArch != PostgresAMD64 && o.pgArch != PostgresARM64 {
			return nil, fmt.Errorf("sqlfn: unknown Postgres architecture %q (want %q or %q)", o.pgArch, PostgresAMD64, PostgresARM64)
		}
		env := pgEnv{libm: pgLibmFor(o.pgArch), contract: o.pgArch == PostgresARM64, clock: o.clock}
		return NewCatalog(append(pgFunctions(env), pgOperators(env)...)...)
	}
	return nil, fmt.Errorf("sqlfn: unknown dialect %q (want %q or %q)", d, SQLite, Postgres)
}

// sqliteBuiltins is SQLite's catalogue: its functions overriding any of the same name.
func sqliteBuiltins(o options) []Func {
	byName := map[string]Func{}
	var order []string
	add := func(fs []Func) {
		for _, f := range fs {
			if _, seen := byName[f.Name()]; !seen {
				order = append(order, f.Name())
			}
			byName[f.Name()] = f
		}
	}
	add(sqliteMath())
	add(sqliteText())
	add(sqliteConditional())
	add(sqliteDates(o.clock))
	add(sqliteJSON())
	add(stackqlExtensions())
	add(sqlitePrintfFuncs())
	add(sqliteOperators())
	out := make([]Func, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

// scalar and table build Funcs from a function value.
type fnDef struct {
	name     string
	min, max int
	shape    Shape
	cols     []string
	hidden   []string
	call     func([]any) (any, error)
}

func (f fnDef) Name() string                 { return f.name }
func (f fnDef) Arity() (int, int)            { return f.min, f.max }
func (f fnDef) Shape() Shape                 { return f.shape }
func (f fnDef) Columns() []string            { return f.cols }
func (f fnDef) Hidden() []string             { return f.hidden }
func (f fnDef) Call(args []any) (any, error) { return f.call(args) }

// NewScalar makes a scalar function; max < 0 accepts any number from min up.
func NewScalar(name string, min, max int, call func([]any) (any, error)) Func {
	return fnDef{name: name, min: min, max: max, shape: Scalar, call: call}
}

// NewTable makes a table function returning rows keyed by cols.
func NewTable(name string, cols []string, min, max int, call func([]any) ([]map[string]any, error)) Func {
	return NewTableHidden(name, cols, nil, min, max, call)
}

// NewTableHidden is NewTable with some of cols hidden.
func NewTableHidden(name string, cols, hidden []string, min, max int, call func([]any) ([]map[string]any, error)) Func {
	return fnDef{name: name, min: min, max: max, shape: Table, cols: cols, hidden: hidden,
		call: func(a []any) (any, error) { return call(a) }}
}

// HiddenColumns is implemented by a table function some of whose columns are hidden: a query may
// name them, but SELECT * leaves them out, as SQLite's HIDDEN virtual-table columns (json_each's
// json and root). They are among its Columns.
type HiddenColumns interface {
	Hidden() []string
}

// Literal is the value a query's literal v has in dialect d. SQLite's literals are as written. To
// Postgres a quoted literal is untyped until a function's parameter types it, an integer literal is
// an integer (a bigint, or a numeric, past those ranges) and a decimal one a numeric.
func Literal(d Dialect, v any) any {
	if d != Postgres {
		return v
	}
	switch x := v.(type) {
	case string:
		return pgUnknown(x)
	case int:
		return pgIntLiteral(int64(x))
	case int64:
		return pgIntLiteral(x)
	case int32:
		return pgInt4(x)
	case float64:
		n, err := pgNumericOf(x)
		if err != nil {
			return v
		}
		return n
	}
	return v
}

func pgIntLiteral(i int64) any {
	if i >= -1<<31 && i < 1<<31 {
		return pgInt4(i)
	}
	return i
}

// Settle is a value as it leaves an expression: plain text where a function returned text that only
// its fellow functions read differently — SQLite's JSON subtype. Table function rows are settled
// column by column.
func Settle(v any) any {
	switch x := v.(type) {
	case jsonText:
		return string(x)
	case pgInt4, pgFloat8, pgNumeric, pgJSON, pgJSONB, pgTextArr, pgInt4Arr:
		return settlePg(x)
	case pgUnknown:
		return string(x)
	case []map[string]any:
		out := make([]map[string]any, len(x))
		for i, r := range x {
			out[i] = settleRow(r)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			if r, ok := e.(map[string]any); ok {
				out[i] = settleRow(r)
			} else {
				out[i] = Settle(e)
			}
		}
		return out
	}
	return v
}

func settleRow(r map[string]any) map[string]any {
	out := make(map[string]any, len(r))
	for k, v := range r {
		out[k] = Settle(v)
	}
	return out
}
