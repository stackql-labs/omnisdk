// Package query is a query as its source language states it, before any document is consulted.
//
// It is language-neutral: a SQL front end and any other produce the same Unresolved, and nothing here
// parses. What a query states is six things — the resources it reads, the columns it names, the
// bindings it makes, the filters it applies, the projection it returns and the form of each join.
// Which binding becomes a request parameter, which an edge between resources and which a filter run
// locally is not stated by the query; it is decided by resolving against the resources' signatures,
// which is another package's job.
//
// Standard library only. A front end depends on this and nothing else of the module.
package query

import (
	"fmt"
	"strings"
)

// Unresolved is a whole query: a read, or a mutation that may also return rows. A mutation differs
// from a read only in that its target's request has an effect; its RETURNING is its Select.
type Unresolved interface {
	// From is the resources in the order the query joins them. The first is the base. A mutation's
	// From is where its values come from, and is empty for INSERT … VALUES.
	From() []Join
	// Where is the conjuncts applied after every join. A mutation's may name its target.
	Where() []Predicate
	// Select is the projection, in output order: a mutation's RETURNING.
	Select() []Output
	// Target is the resource a mutation changes; nil for a read.
	Target() Target
}

// Verb is what a mutation does to its target.
type Verb int

const (
	Insert Verb = iota + 1
	Update
	Delete
)

func (v Verb) String() string {
	switch v {
	case Insert:
		return "insert"
	case Update:
		return "update"
	case Delete:
		return "delete"
	}
	return fmt.Sprintf("Verb(%d)", int(v))
}

// Target is a mutation's resource, what it does, and the values it sets.
type Target interface {
	Verb() Verb
	Resource() Resource
	// Set is INSERT's columns and values, or UPDATE's SET; empty for DELETE. For a multi-row INSERT it
	// is the first row.
	Set() []Assignment
	// Rows is every row an INSERT … VALUES writes; one row, Set, for anything else.
	Rows() [][]Assignment
}

// Assignment is one column set to a value, which may read the mutation's From.
type Assignment interface {
	Column() string
	Value() Expr
}

// JoinForm is how a resource's rows combine with those before it.
type JoinForm int

const (
	// Base is the first resource: nothing precedes it to join with.
	Base JoinForm = iota
	// Inner keeps a row only where the join's predicates hold.
	Inner
	// Left keeps every row before it, with this resource's columns absent where nothing matched.
	Left
)

func (f JoinForm) String() string {
	switch f {
	case Base:
		return "base"
	case Inner:
		return "inner"
	case Left:
		return "left"
	}
	return fmt.Sprintf("JoinForm(%d)", int(f))
}

// Resource is one reference to a resource: its handle and the alias naming this use of it.
type Resource interface {
	// Alias names this reference; the handle when the query gave none.
	Alias() string
	// Handle is the resource as the query named it, e.g. "aws.iam.users".
	Handle() string
}

// Join is one resource's place in the FROM clause. ON is kept with its join rather than merged into
// WHERE: under a left join the two differ, since a failed ON keeps the row and a failed WHERE drops it.
type Join interface {
	Resource() Resource
	Form() JoinForm
	On() []Predicate
}

// Output is one projected column.
type Output interface {
	Name() string
	Expr() Expr
}

// Expr is a value: a literal, a collection of values, a column, or a function applied to values.
// The concrete kinds are the interfaces below; a consumer distinguishes them with a type switch.
type Expr interface{ expr() }

// Literal is a constant scalar.
type Literal interface {
	Expr
	Value() any
}

// Collection is a constant list of values, e.g. the right side of IN.
type Collection interface {
	Expr
	Items() []Expr
}

// Column is a column handle. Qualifier is the alias it names, empty where the query did not qualify
// it — which is then ambiguous until resolved against the resources' columns.
type Column interface {
	Expr
	Qualifier() string
	Name() string
}

// Star is every column of a resource, or of every resource where Qualifier is empty: SELECT * and
// SELECT u.*. It is an output only, and which columns it means is known on resolution.
type Star interface {
	Expr
	Qualifier() string
	star()
}

// Call is a function applied to arguments. Whether it yields a scalar or rows is the function's
// signature, known on resolution.
type Call interface {
	Expr
	Func() string
	Args() []Expr
}

// Predicate is a condition. As with Expr, the kinds are the interfaces below.
type Predicate interface{ predicate() }

// CompareOp is a comparison operator.
type CompareOp string

const (
	Eq CompareOp = "="
	Ne CompareOp = "<>"
	Lt CompareOp = "<"
	Le CompareOp = "<="
	Gt CompareOp = ">"
	Ge CompareOp = ">="
)

// Compare is Left op Right. An Eq with a column on one side is a candidate binding.
type Compare interface {
	Predicate
	Op() CompareOp
	Left() Expr
	Right() Expr
}

// In is Expr IN Set. With a column on the left it is a candidate collection binding.
type In interface {
	Predicate
	Expr() Expr
	Set() Expr
}

// Test is a boolean-valued expression used as a condition, e.g. like(name, 'a%').
type Test interface {
	Predicate
	Cond() Expr
}

// Or holds when any of its predicates holds. It never binds.
type Or interface {
	Predicate
	Any() []Predicate
}

// Not holds when its predicate does not. It never binds.
type Not interface {
	Predicate
	Negated() Predicate
}

// New validates and returns a query. It rejects what is wrong in the query itself, before any
// signature is consulted: a first join that is not the base, an alias used twice, a column
// qualified by an alias the query does not have in scope, and an output name used twice.
func New(from []Join, where []Predicate, sel []Output) (Unresolved, error) {
	if len(from) == 0 {
		return nil, fmt.Errorf("query: no resources")
	}
	return build(nil, from, where, sel)
}

// NewMutation validates and returns a mutation. Its values may read from; its WHERE and RETURNING
// may also name the target.
func NewMutation(t Target, from []Join, where []Predicate, returning []Output) (Unresolved, error) {
	switch {
	case t == nil:
		return nil, fmt.Errorf("query: a mutation needs a target")
	case t.Verb() != Insert && t.Verb() != Update && t.Verb() != Delete:
		return nil, fmt.Errorf("query: unknown verb %s", t.Verb())
	case t.Resource().Alias() == "":
		return nil, fmt.Errorf("query: the target has neither an alias nor a handle")
	case t.Verb() == Delete && len(t.Set()) > 0:
		return nil, fmt.Errorf("query: a delete sets nothing")
	case t.Verb() != Delete && len(t.Set()) == 0:
		return nil, fmt.Errorf("query: an %s sets no columns", t.Verb())
	}
	if rows := t.Rows(); len(rows) > 1 {
		first := columnsOf(rows[0])
		for i, row := range rows[1:] {
			if got := columnsOf(row); got != first {
				return nil, fmt.Errorf("query: VALUES row %d sets (%s), row 1 sets (%s)", i+2, got, first)
			}
		}
	}
	return build(t, from, where, returning)
}

func build(t Target, from []Join, where []Predicate, sel []Output) (Unresolved, error) {
	scope := map[string]bool{}
	for i, j := range from {
		switch {
		case i == 0 && j.Form() != Base:
			return nil, fmt.Errorf("query: the first resource is the base, not a %s join", j.Form())
		case i > 0 && j.Form() == Base:
			return nil, fmt.Errorf("query: %s is joined as a base; only the first resource is", j.Resource().Alias())
		case j.Resource().Alias() == "":
			return nil, fmt.Errorf("query: a resource has neither an alias nor a handle")
		case scope[j.Resource().Alias()]:
			return nil, fmt.Errorf("query: %q is referenced twice; give each reference an alias", j.Resource().Alias())
		}
		scope[j.Resource().Alias()] = true
		// ON may name this resource and those before it, not those joined later.
		for _, p := range j.On() {
			if err := inScope(p, scope); err != nil {
				return nil, fmt.Errorf("query: ON of %s: %w", j.Resource().Alias(), err)
			}
		}
	}
	if t != nil {
		alias := t.Resource().Alias()
		if scope[alias] {
			return nil, fmt.Errorf("query: %q is both the target and a source; give each reference an alias", alias)
		}
		// Values read the sources, never the row being written.
		cols := map[string]bool{}
		for _, a := range t.Set() {
			switch {
			case a.Column() == "":
				return nil, fmt.Errorf("query: an assignment names no column")
			case cols[a.Column()]:
				return nil, fmt.Errorf("query: %q is set twice", a.Column())
			}
			cols[a.Column()] = true
			if err := exprInScope(a.Value(), scope); err != nil {
				return nil, fmt.Errorf("query: %s = …: %w", a.Column(), err)
			}
		}
		scope[alias] = true
	}
	for _, p := range where {
		if err := inScope(p, scope); err != nil {
			return nil, fmt.Errorf("query: WHERE: %w", err)
		}
	}
	names := map[string]bool{}
	for _, o := range sel {
		if st, ok := o.Expr().(Star); ok {
			if q := st.Qualifier(); q != "" && !scope[q] {
				return nil, fmt.Errorf("query: %s.* names %q, which is not in scope", q, q)
			}
			continue
		}
		switch {
		case o.Name() == "":
			return nil, fmt.Errorf("query: an output has no name")
		case names[o.Name()]:
			return nil, fmt.Errorf("query: output %q is named twice; alias one of them", o.Name())
		}
		names[o.Name()] = true
		if err := exprInScope(o.Expr(), scope); err != nil {
			return nil, fmt.Errorf("query: output %q: %w", o.Name(), err)
		}
	}
	return unresolved{from: from, where: where, sel: sel, target: t}, nil
}

func inScope(p Predicate, scope map[string]bool) error {
	switch p := p.(type) {
	case Compare:
		if err := exprInScope(p.Left(), scope); err != nil {
			return err
		}
		return exprInScope(p.Right(), scope)
	case In:
		if err := exprInScope(p.Expr(), scope); err != nil {
			return err
		}
		return exprInScope(p.Set(), scope)
	case Test:
		return exprInScope(p.Cond(), scope)
	case Or:
		for _, q := range p.Any() {
			if err := inScope(q, scope); err != nil {
				return err
			}
		}
	case Not:
		return inScope(p.Negated(), scope)
	}
	return nil
}

func exprInScope(e Expr, scope map[string]bool) error {
	switch e := e.(type) {
	case Column:
		if q := e.Qualifier(); q != "" && !scope[q] {
			return fmt.Errorf("%s.%s names %q, which is not in scope", q, e.Name(), q)
		}
	case Collection:
		for _, x := range e.Items() {
			if err := exprInScope(x, scope); err != nil {
				return err
			}
		}
	case Call:
		for _, x := range e.Args() {
			if err := exprInScope(x, scope); err != nil {
				return err
			}
		}
	}
	return nil
}

type unresolved struct {
	from   []Join
	where  []Predicate
	sel    []Output
	target Target
}

func (u unresolved) Target() Target { return u.target }

func (u unresolved) From() []Join       { return u.from }
func (u unresolved) Where() []Predicate { return u.where }
func (u unresolved) Select() []Output   { return u.sel }

// NewInsert targets r with INSERT; set is its columns and values.
func NewInsert(r Resource, set ...Assignment) Target { return target{verb: Insert, r: r, set: set} }

// NewInsertRows targets r with a multi-row INSERT … VALUES: one assignment set per row, each setting
// the same columns.
func NewInsertRows(r Resource, rows ...[]Assignment) Target {
	t := target{verb: Insert, r: r, rows: rows}
	if len(rows) > 0 {
		t.set = rows[0]
	}
	return t
}

// columnsOf is a row's column list, for comparing rows.
func columnsOf(row []Assignment) string {
	names := make([]string, 0, len(row))
	for _, a := range row {
		names = append(names, a.Column())
	}
	return strings.Join(names, ", ")
}

// NewUpdate targets r with UPDATE … SET.
func NewUpdate(r Resource, set ...Assignment) Target { return target{verb: Update, r: r, set: set} }

// NewDelete targets r with DELETE.
func NewDelete(r Resource) Target { return target{verb: Delete, r: r} }

type target struct {
	verb Verb
	r    Resource
	set  []Assignment
	rows [][]Assignment
}

func (t target) Rows() [][]Assignment {
	if len(t.rows) > 0 {
		return t.rows
	}
	if t.set == nil {
		return nil
	}
	return [][]Assignment{t.set}
}

func (t target) Verb() Verb         { return t.verb }
func (t target) Resource() Resource { return t.r }
func (t target) Set() []Assignment  { return t.set }

// NewAssignment sets column to value.
func NewAssignment(column string, value Expr) Assignment { return assignment{col: column, v: value} }

type assignment struct {
	col string
	v   Expr
}

func (a assignment) Column() string { return a.col }
func (a assignment) Value() Expr    { return a.v }

// NewResource references a resource. An empty alias is the handle, as an unaliased SQL table is
// referenced by its own name.
func NewResource(alias, handle string) Resource {
	if alias == "" {
		alias = handle
	}
	return resource{alias: alias, handle: handle}
}

type resource struct{ alias, handle string }

func (r resource) Alias() string  { return r.alias }
func (r resource) Handle() string { return r.handle }

// NewJoin places a resource in FROM.
func NewJoin(r Resource, form JoinForm, on ...Predicate) Join {
	return join{r: r, form: form, on: on}
}

type join struct {
	r    Resource
	form JoinForm
	on   []Predicate
}

func (j join) Resource() Resource { return j.r }
func (j join) Form() JoinForm     { return j.form }
func (j join) On() []Predicate    { return j.on }

// NewOutput projects an expression under a name.
func NewOutput(name string, e Expr) Output { return output{name: name, e: e} }

type output struct {
	name string
	e    Expr
}

func (o output) Name() string { return o.name }
func (o output) Expr() Expr   { return o.e }

// NewLiteral is a constant scalar.
func NewLiteral(v any) Literal { return literal{v: v} }

type literal struct{ v any }

func (literal) expr()        {}
func (l literal) Value() any { return l.v }

// NewCollection is a constant list.
func NewCollection(items ...Expr) Collection { return collection{items: items} }

type collection struct{ items []Expr }

func (collection) expr()           {}
func (c collection) Items() []Expr { return c.items }

// NewColumn is a column handle; qualifier may be empty.
func NewColumn(qualifier, name string) Column { return column{q: qualifier, name: name} }

type column struct{ q, name string }

func (column) expr()               {}
func (c column) Qualifier() string { return c.q }
func (c column) Name() string      { return c.name }

// NewStar is every column of the resource aliased qualifier, or of every resource where it is empty.
// Its output takes its names from the columns, so the Output's own name is ignored.
func NewStar(qualifier string) Star { return star{q: qualifier} }

type star struct{ q string }

func (star) expr()               {}
func (star) star()               {}
func (s star) Qualifier() string { return s.q }

// NewCall applies a function.
func NewCall(fn string, args ...Expr) Call { return call{fn: fn, args: args} }

type call struct {
	fn   string
	args []Expr
}

func (call) expr()          {}
func (c call) Func() string { return c.fn }
func (c call) Args() []Expr { return c.args }

// NewCompare is l op r.
func NewCompare(op CompareOp, l, r Expr) Compare { return compare{op: op, l: l, r: r} }

// NewEq is l = r.
func NewEq(l, r Expr) Compare { return NewCompare(Eq, l, r) }

type compare struct {
	op   CompareOp
	l, r Expr
}

func (compare) predicate()      {}
func (c compare) Op() CompareOp { return c.op }
func (c compare) Left() Expr    { return c.l }
func (c compare) Right() Expr   { return c.r }

// NewIn is e IN set.
func NewIn(e, set Expr) In { return in{e: e, set: set} }

type in struct{ e, set Expr }

func (in) predicate()   {}
func (p in) Expr() Expr { return p.e }
func (p in) Set() Expr  { return p.set }

// NewTest uses a boolean expression as a condition.
func NewTest(e Expr) Test { return test{e: e} }

type test struct{ e Expr }

func (test) predicate()   {}
func (t test) Cond() Expr { return t.e }

// NewOr holds when any of ps holds.
func NewOr(ps ...Predicate) Or { return or{ps: ps} }

type or struct{ ps []Predicate }

func (or) predicate()         {}
func (o or) Any() []Predicate { return o.ps }

// NewNot negates p.
func NewNot(p Predicate) Not { return not{p: p} }

type not struct{ p Predicate }

func (not) predicate()           {}
func (n not) Negated() Predicate { return n.p }
