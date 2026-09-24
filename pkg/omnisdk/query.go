package omnisdk

import (
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/fn"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

// Table is what the documents say about one resource: the address it resolves to and the select
// methods it can run. It is what a query cannot state and resolution needs.
type Table interface {
	Address() string
	Methods() []MethodSignature
}

// MethodSignature is one select method, as its document declares it: what it takes and the columns
// each row has.
type MethodSignature interface {
	Method() string
	Params() []ParamSignature
	// Columns are the row's declared properties, empty where the document states no schema.
	Columns() []string
}

// ParamSignature is one declared input.
type ParamSignature interface {
	Name() string
	Required() bool
}

// DescribeTable reads what the documents under dir say about an address.
func DescribeTable(dir, address string) (Table, error) {
	c, err := openDocs(dir, address)
	if err != nil {
		return nil, err
	}
	ops, err := c.Operations(address, "select")
	if err != nil {
		return nil, err
	}
	methods := make([]MethodSignature, 0, len(ops))
	for _, ex := range ops {
		methods = append(methods, docSignature{ex: ex})
	}
	return table{address: address, methods: methods}, nil
}

type table struct {
	address string
	methods []MethodSignature
}

func (t table) Address() string            { return t.address }
func (t table) Methods() []MethodSignature { return t.methods }

type docSignature struct{ ex aot.AOTExchange }

func (s docSignature) Method() string { return s.ex.Name() }

func (s docSignature) Params() []ParamSignature {
	var out []ParamSignature
	for _, p := range s.ex.Request().Parameters() {
		out = append(out, p)
	}
	return out
}

// Columns walks the response schema to the row: down the object key, then into an array's items.
func (s docSignature) Columns() []string {
	resp := s.ex.Response()
	sch := resp.Schema()
	if sch == nil {
		return nil
	}
	for _, seg := range strings.Split(strings.TrimPrefix(strings.TrimPrefix(resp.ObjectKey(), "$"), "."), ".") {
		if seg == "" {
			continue
		}
		next, ok := sch.Property(seg)
		if !ok {
			break
		}
		sch = next
	}
	if items, ok := sch.Items(); ok {
		sch = items
	}
	return sch.Properties()
}

// Resolution is a query resolved against its tables: the graph to run and the query-wide params.
type Resolution interface {
	Graph() Graph
	// Params are constant bindings no single table owns: request inputs for every node taking them.
	Params() map[string]string
}

type resolution struct {
	graph  Graph
	params map[string]string
}

func (r resolution) Graph() Graph              { return r.graph }
func (r resolution) Params() map[string]string { return r.params }

// Resolve places every part of a query, given each alias's table. A binding becomes a request
// parameter or an edge according to what the tables' methods take and require; the direction of an
// edge is the side that cannot run without it. Anything that cannot be placed is an error naming it,
// never a guess.
func Resolve(q query.Unresolved, tables map[string]Table) (Resolution, error) {
	r := resolver{tables: map[string]Table{}, params: map[string]string{}, nodeParams: map[string]map[string]string{}}
	var conjuncts []query.Predicate
	for _, j := range q.From() {
		alias := j.Resource().Alias()
		t, ok := tables[alias]
		if !ok {
			return nil, fmt.Errorf("omnisdk: no table described for %q", alias)
		}
		if j.Form() == query.Left {
			return nil, fmt.Errorf("omnisdk: %s: a left join is not yet supported", alias)
		}
		r.tables[alias] = t
		r.order = append(r.order, alias)
		// Under an inner join ON and WHERE are the same filter.
		conjuncts = append(conjuncts, j.On()...)
	}
	conjuncts = append(conjuncts, q.Where()...)

	// Constant bindings first: they decide what each table can run without the others.
	var joins []query.Compare
	for _, p := range conjuncts {
		c, ok := p.(query.Compare)
		if !ok || c.Op() != query.Eq {
			return nil, fmt.Errorf("omnisdk: %s: only equality is supported; client-side filtering is not yet", describe(p))
		}
		col, lit, ok := columnAndLiteral(c)
		if !ok {
			joins = append(joins, c)
			continue
		}
		if err := r.bindConstant(c, col, lit); err != nil {
			return nil, err
		}
	}
	for _, c := range joins {
		if err := r.bindJoin(c); err != nil {
			return nil, err
		}
	}
	return r.build(q.Select())
}

type resolver struct {
	tables     map[string]Table
	order      []string
	params     map[string]string
	nodeParams map[string]map[string]string
	arrivals   map[string][]arrival
	consumers  []string
	retain     map[string][]string
}

type arrival struct{ from, src, as string }

// bindConstant places column = literal. A qualified column binds its table's parameter. An
// unqualified one binds the one table that has it; where none or several do, it is query-wide, as a
// scope value such as region is.
func (r *resolver) bindConstant(c query.Compare, col query.Column, lit query.Literal) error {
	alias := col.Qualifier()
	if alias == "" {
		owners := r.owners(col.Name())
		if len(owners) != 1 {
			r.params[col.Name()] = fmt.Sprint(lit.Value())
			return nil
		}
		alias = owners[0]
	}
	if !accepts(r.tables[alias], col.Name()) {
		return fmt.Errorf("omnisdk: %s: %s takes no %q; client-side filtering is not yet supported", describe(c), alias, col.Name())
	}
	if r.nodeParams[alias] == nil {
		r.nodeParams[alias] = map[string]string{}
	}
	r.nodeParams[alias][col.Name()] = fmt.Sprint(lit.Value())
	return nil
}

// bindJoin places column = column across two tables as an edge onto the side that needs the value.
func (r *resolver) bindJoin(c query.Compare) error {
	l, lok := c.Left().(query.Column)
	rc, rok := c.Right().(query.Column)
	if !lok || !rok {
		return fmt.Errorf("omnisdk: %s: a computed join value is not yet supported", describe(c))
	}
	var err error
	if l, err = r.qualify(l); err != nil {
		return fmt.Errorf("omnisdk: %s: %w", describe(c), err)
	}
	if rc, err = r.qualify(rc); err != nil {
		return fmt.Errorf("omnisdk: %s: %w", describe(c), err)
	}
	if l.Qualifier() == rc.Qualifier() {
		return fmt.Errorf("omnisdk: %s: both sides are %s; client-side filtering is not yet supported", describe(c), l.Qualifier())
	}
	lNeeds := needs(r.tables[l.Qualifier()], l.Name(), r.known(l.Qualifier()))
	rNeeds := needs(r.tables[rc.Qualifier()], rc.Name(), r.known(rc.Qualifier()))
	var to, from query.Column
	switch {
	case lNeeds && rNeeds:
		return fmt.Errorf("omnisdk: %s: %s and %s each need the other", describe(c), l.Qualifier(), rc.Qualifier())
	case lNeeds:
		to, from = l, rc
	case rNeeds:
		to, from = rc, l
	default:
		return fmt.Errorf("omnisdk: %s: neither side needs the other's value; a client-side join is not yet supported", describe(c))
	}
	if r.arrivals == nil {
		r.arrivals, r.retain = map[string][]arrival{}, map[string][]string{}
	}
	if _, seen := r.arrivals[to.Qualifier()]; !seen {
		r.consumers = append(r.consumers, to.Qualifier())
	}
	r.arrivals[to.Qualifier()] = append(r.arrivals[to.Qualifier()], arrival{from: from.Qualifier(), src: from.Name(), as: to.Name()})
	r.retain[from.Qualifier()] = append(r.retain[from.Qualifier()], from.Name())
	return nil
}

// build assembles the graph: nodes with their bound params, one projection per table carrying its
// share of the select list plus what its edges read, and the wirings.
func (r *resolver) build(sel []query.Output) (Resolution, error) {
	byAlias := map[string][]SelectColumn{}
	for _, o := range sel {
		alias, e, err := r.projected(o.Expr())
		if err != nil {
			return nil, fmt.Errorf("omnisdk: output %q: %w", o.Name(), err)
		}
		byAlias[alias] = append(byAlias[alias], NewSelectColumn(o.Name(), e))
	}
	var nodes []Node
	var projections []Projection
	for _, alias := range r.order {
		nodes = append(nodes, NewNode(alias, r.tables[alias].Address(), r.nodeParams[alias]))
		cols := byAlias[alias]
		// A column an edge reads is kept even where the query never selected it: the projection
		// replaces the row, and the edge reads it afterwards. Kept under a hidden name, so it is
		// dropped from the output the query asked for.
		for _, name := range r.retain[alias] {
			if !selects(cols, name) {
				cols = append(cols, NewSelectColumn(kept(name), NewField(name)))
			}
		}
		if len(cols) == 0 {
			continue
		}
		p, err := NewProjection(alias, cols)
		if err != nil {
			return nil, err
		}
		projections = append(projections, p)
	}
	var wirings []Wiring
	for _, to := range r.consumers {
		var in []Inbound
		for _, a := range r.arrivals[to] {
			src := a.src
			if !selects(byAlias[a.from], src) {
				src = kept(src)
			}
			in = append(in, NewInbound(a.from, src, a.as))
		}
		wirings = append(wirings, NewWiring(to, in, "", ""))
	}
	g, err := NewGraphWithProjections(nodes, wirings, projections)
	if err != nil {
		return nil, err
	}
	return resolution{graph: g, params: r.params}, nil
}

// projected converts an output expression to the engine's, reporting the one table it reads.
func (r *resolver) projected(e query.Expr) (string, Expression, error) {
	var alias string
	var conv func(query.Expr) (fn.Expr, error)
	conv = func(e query.Expr) (fn.Expr, error) {
		switch e := e.(type) {
		case query.Literal:
			return fn.Literal(e.Value()), nil
		case query.Column:
			c, err := r.qualify(e)
			if err != nil {
				return nil, err
			}
			if alias != "" && alias != c.Qualifier() {
				return nil, fmt.Errorf("reads %s and %s; an expression across tables is not yet supported", alias, c.Qualifier())
			}
			alias = c.Qualifier()
			return fn.Field(c.Name()), nil
		case query.Call:
			args := make([]fn.Expr, 0, len(e.Args()))
			for _, a := range e.Args() {
				x, err := conv(a)
				if err != nil {
					return nil, err
				}
				args = append(args, x)
			}
			return fn.Call(e.Func(), args...), nil
		}
		return nil, fmt.Errorf("%T cannot be projected", e)
	}
	x, err := conv(e)
	if err != nil {
		return "", nil, err
	}
	if alias == "" {
		return "", nil, fmt.Errorf("reads no table")
	}
	return alias, expression{e: x}, nil
}

// qualify gives an unqualified column the one table whose rows have it.
func (r *resolver) qualify(c query.Column) (query.Column, error) {
	if c.Qualifier() != "" {
		return c, nil
	}
	switch owners := r.owners(c.Name()); len(owners) {
	case 1:
		return query.NewColumn(owners[0], c.Name()), nil
	case 0:
		return nil, fmt.Errorf("no table has a column %q", c.Name())
	default:
		return nil, fmt.Errorf("%q is ambiguous between %v; qualify it", c.Name(), owners)
	}
}

// owners are the aliases whose tables have name as a column or a parameter.
func (r *resolver) owners(name string) []string {
	var out []string
	for _, alias := range r.order {
		if hasColumn(r.tables[alias], name) || accepts(r.tables[alias], name) {
			out = append(out, alias)
		}
	}
	return out
}

// known is what a table has bound before any edge: query-wide params and its own.
func (r *resolver) known(alias string) map[string]bool {
	k := map[string]bool{}
	for name := range r.params {
		k[name] = true
	}
	for name := range r.nodeParams[alias] {
		k[name] = true
	}
	return k
}

func hasColumn(t Table, name string) bool {
	for _, m := range t.Methods() {
		if contains(m.Columns(), name) {
			return true
		}
	}
	return false
}

// accepts reports whether any method takes the parameter.
func accepts(t Table, name string) bool {
	for _, m := range t.Methods() {
		for _, p := range m.Params() {
			if p.Name() == name {
				return true
			}
		}
	}
	return false
}

// needs reports whether the table cannot run on known alone and name would help: no method is
// satisfiable from known, and some method takes name.
func needs(t Table, name string, known map[string]bool) bool {
	if !accepts(t, name) {
		return false
	}
	for _, m := range t.Methods() {
		ok := true
		for _, p := range m.Params() {
			if p.Required() && !known[p.Name()] {
				ok = false
				break
			}
		}
		if ok {
			return false
		}
	}
	return true
}

func columnAndLiteral(c query.Compare) (query.Column, query.Literal, bool) {
	if col, ok := c.Left().(query.Column); ok {
		if lit, ok := c.Right().(query.Literal); ok {
			return col, lit, true
		}
	}
	if col, ok := c.Right().(query.Column); ok {
		if lit, ok := c.Left().(query.Literal); ok {
			return col, lit, true
		}
	}
	return nil, nil, false
}

func describe(p query.Predicate) string {
	if c, ok := p.(query.Compare); ok {
		return describeExpr(c.Left()) + " " + string(c.Op()) + " " + describeExpr(c.Right())
	}
	return fmt.Sprintf("%T", p)
}

func describeExpr(e query.Expr) string {
	switch e := e.(type) {
	case query.Column:
		if e.Qualifier() == "" {
			return e.Name()
		}
		return e.Qualifier() + "." + e.Name()
	case query.Literal:
		return fmt.Sprintf("%q", fmt.Sprint(e.Value()))
	case query.Call:
		var args []string
		for _, a := range e.Args() {
			args = append(args, describeExpr(a))
		}
		return e.Func() + "(" + strings.Join(args, ", ") + ")"
	}
	return fmt.Sprintf("%T", e)
}

// kept names a column retained for an edge but not selected; egress drops it.
func kept(name string) string { return "\x00" + name }

func selects(cols []SelectColumn, name string) bool {
	for _, c := range cols {
		if c.Out() == name {
			return true
		}
	}
	return false
}
