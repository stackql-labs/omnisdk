package omnisdk

import (
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/system_g/fn"
)

// Expression is one value in a query: a literal, a column of a node's row, or a function applied to
// other expressions. There are no operators and no parser — the caller states the tree, so nothing
// has to decide what an unquoted word meant.
type Expression interface {
	// Deps are the aliases whose rows it reads. Empty means it is fixed before any request is made.
	Deps() []string
	// internal converts to the engine's expression; unexported so the tree cannot be implemented
	// outside this package, where it would bypass function resolution.
	internal() fn.Expr
}

type expression struct {
	e fn.Expr
	// What analysis needs to see through the engine form: a literal's value, a column's alias and
	// name, a call's name and arguments.
	lit         any
	alias, name string
	call        string
	args        []Expression
}

func (x expression) internal() fn.Expr { return x.e }

func (x expression) Deps() []string {
	if x.alias != "" {
		return []string{x.alias}
	}
	var out []string
	for _, a := range x.args {
		for _, d := range a.Deps() {
			if !contains(out, d) {
				out = append(out, d)
			}
		}
	}
	return out
}

// NewLiteral is a constant value.
func NewLiteral(v any) Expression { return expression{e: fn.Literal(v), lit: v} }

// NewField reads a column of the current row, unqualified. A missing column is NULL, not an error:
// provider rows are ragged, and a select that failed on the first absent field would be unusable.
func NewField(name string) Expression { return expression{e: fn.Field(name), name: name} }

// NewColumn reads a column of the named node's row: alias.name.
func NewColumn(alias, name string) Expression {
	return expression{e: fn.Field(name), alias: alias, name: name}
}

// NewCall applies a registered function. Row-producing functions are allowed and fan the row out;
// at most one per select list.
func NewCall(name string, args ...Expression) Expression {
	in := make([]fn.Expr, 0, len(args))
	for _, a := range args {
		in = append(in, a.internal())
	}
	return expression{e: fn.Call(name, in...), call: name, args: args}
}

// SelectColumn is one output column: the name it is emitted under and the expression producing it.
type SelectColumn interface {
	Out() string
	Expr() Expression
}

// NewSelectColumn declares an output column.
func NewSelectColumn(out string, e Expression) SelectColumn { return selectColumn{out: out, e: e} }

type selectColumn struct {
	out string
	e   Expression
}

func (c selectColumn) Out() string      { return c.out }
func (c selectColumn) Expr() Expression { return c.e }

// Projection is a select list applied to one node's rows. It replaces the row: a column the
// select does not name is not emitted, which is what makes it a projection rather than an
// annotation.
type Projection interface {
	// Alias is the node whose rows it shapes.
	Alias() string
	Columns() []SelectColumn
}

// NewProjection declares a projection over a node, named by its alias.
func NewProjection(alias string, cols []SelectColumn) (Projection, error) {
	if alias == "" {
		return nil, fmt.Errorf("omnisdk: projection needs an alias")
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("omnisdk: projection on %s selects no columns", alias)
	}
	for _, c := range cols {
		if c.Out() == "" {
			return nil, fmt.Errorf("omnisdk: projection on %s has a column with no output name", alias)
		}
	}
	return projection{alias: alias, cols: cols}, nil
}

type projection struct {
	alias string
	cols  []SelectColumn
}

func (p projection) Alias() string           { return p.alias }
func (p projection) Columns() []SelectColumn { return p.cols }

// columns converts a projection to the engine's select list.
func (p projection) internal() []fn.Column {
	out := make([]fn.Column, 0, len(p.cols))
	for _, c := range p.cols {
		out = append(out, fn.Column{Out: c.Out(), Expr: c.Expr().internal()})
	}
	return out
}
