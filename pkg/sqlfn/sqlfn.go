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

// Builtins is every function this package ships.
func Builtins() Catalog {
	c, err := NewCatalog(builtins()...)
	if err != nil {
		panic(err) // a duplicate here is a bug in this package, not in any input
	}
	return c
}

// scalar and table build Funcs from a function value.
type fnDef struct {
	name     string
	min, max int
	shape    Shape
	cols     []string
	call     func([]any) (any, error)
}

func (f fnDef) Name() string                 { return f.name }
func (f fnDef) Arity() (int, int)            { return f.min, f.max }
func (f fnDef) Shape() Shape                 { return f.shape }
func (f fnDef) Columns() []string            { return f.cols }
func (f fnDef) Call(args []any) (any, error) { return f.call(args) }

// NewScalar makes a scalar function; max < 0 accepts any number from min up.
func NewScalar(name string, min, max int, call func([]any) (any, error)) Func {
	return fnDef{name: name, min: min, max: max, shape: Scalar, call: call}
}

// NewTable makes a table function returning rows keyed by cols.
func NewTable(name string, cols []string, min, max int, call func([]any) ([]map[string]any, error)) Func {
	return fnDef{name: name, min: min, max: max, shape: Table, cols: cols,
		call: func(a []any) (any, error) { return call(a) }}
}
