package fn

import (
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Expr is one value in a select list or an input list: a literal, a field of the current row, or a
// function applied to other expressions. Three cases, no operators and no parser — the caller states
// the tree, so nothing here has to decide what an unquoted word meant.
type Expr interface {
	// Eval computes the value against one row.
	Eval(row map[string]any, r facade.FnRegistry) (any, error)
	// Fn names the function at the root of this expression, empty when it is not a call. A caller
	// needs this to know whether the expression can produce rows before it evaluates anything.
	Fn() string
}

// Literal is a constant.
func Literal(v any) Expr { return literal{v: v} }

type literal struct{ v any }

func (e literal) Eval(map[string]any, facade.FnRegistry) (any, error) { return e.v, nil }
func (e literal) Fn() string                                          { return "" }

// Field reads a key of the current row. A missing key is NULL, not an error: rows off a provider are
// ragged, and a select that failed on the first absent field would be unusable.
func Field(name string) Expr { return field{name: name} }

type field struct{ name string }

func (e field) Eval(row map[string]any, _ facade.FnRegistry) (any, error) { return row[e.name], nil }
func (e field) Fn() string                                                { return "" }

// Call applies a registered function to evaluated arguments.
func Call(name string, args ...Expr) Expr { return call{name: name, args: args} }

type call struct {
	name string
	args []Expr
}

func (e call) Fn() string { return e.name }

func (e call) Eval(row map[string]any, r facade.FnRegistry) (any, error) {
	args := make([]any, 0, len(e.args))
	for _, a := range e.args {
		v, err := a.Eval(row, r)
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	return Invoke(r, e.name, args)
}

// Column is one output column of a select list: the name it is emitted under and the expression
// that produces it.
type Column struct {
	Out  string
	Expr Expr
}

// RowProducing reports whether name is registered and every signature of it yields rows. A function
// that yields rows under one signature and a scalar under another is rejected: which one applies is
// not known until the arguments are evaluated, and a select list's shape has to be known before
// that.
func RowProducing(r facade.FnRegistry, name string) (bool, error) {
	f, ok := r.Fn(name)
	if !ok {
		return false, fmt.Errorf("fn: no function %q", name)
	}
	rows, scalars := 0, 0
	for _, s := range f.Signatures() {
		if len(s.Columns()) > 0 {
			rows++
		} else {
			scalars++
		}
	}
	if rows > 0 && scalars > 0 {
		return false, fmt.Errorf("fn: %s yields rows under some signatures and a scalar under others", name)
	}
	return rows > 0, nil
}
