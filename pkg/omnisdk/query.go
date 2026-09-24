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
// parameter, a fan-out or an edge according to what the tables' methods take and require; the
// direction of an edge is the side that cannot run without it. Every other condition, and every
// binding the row can also be checked against, is a filter on the returned rows. Anything that
// cannot be placed is an error naming it, never a guess.
func Resolve(q query.Unresolved, tables map[string]Table) (Resolution, error) {
	r := resolver{tables: map[string]Table{}, params: map[string]string{}, nodeParams: map[string]map[string]string{},
		fanout: map[string]map[string][]string{}, wide: map[string][]string{}, arrivals: map[string][]arrival{}, retain: map[string][]string{},
		computed: map[string][]SelectColumn{}}
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
	var rest []query.Predicate
	for _, p := range conjuncts {
		placed, err := r.bindConstant(p)
		if err != nil {
			return nil, err
		}
		if !placed {
			rest = append(rest, p)
		}
	}
	for _, p := range rest {
		placed, err := r.bindJoin(p)
		if err != nil {
			return nil, err
		}
		if !placed {
			if err := r.filter(p); err != nil {
				return nil, err
			}
		}
	}
	return r.build(q.Select())
}

type resolver struct {
	tables     map[string]Table
	order      []string
	params     map[string]string
	nodeParams map[string]map[string]string
	fanout     map[string]map[string][]string
	wide       map[string][]string
	arrivals   map[string][]arrival
	consumers  []string
	retain     map[string][]string
	computed   map[string][]SelectColumn
	filters    []query.Predicate
	finals     []query.Output
}

type arrival struct{ from, src, as string }

// bindConstant places column = literal and column IN (literals). A qualified column binds its
// table's parameter. An unqualified one binds the one table that has it; where none or several do,
// it is query-wide, as a scope value such as region is. A column the table does not take is not
// placed here, and becomes a filter. A placed binding on a column the row also carries is checked
// again on the rows, since a request parameter is not always applied exactly.
func (r *resolver) bindConstant(p query.Predicate) (bool, error) {
	var col query.Column
	var values []string
	switch p := p.(type) {
	case query.Compare:
		c, lit, ok := columnAndLiteral(p)
		if !ok || p.Op() != query.Eq {
			return false, nil
		}
		col, values = c, []string{fmt.Sprint(lit.Value())}
	case query.In:
		c, ok := p.Expr().(query.Column)
		set, isSet := p.Set().(query.Collection)
		if !ok || !isSet {
			return false, nil
		}
		for _, x := range set.Items() {
			lit, ok := x.(query.Literal)
			if !ok {
				return false, nil
			}
			values = append(values, fmt.Sprint(lit.Value()))
		}
		col = c
	default:
		return false, nil
	}
	alias := col.Qualifier()
	if alias == "" {
		owners := r.owners(col.Name())
		if len(owners) != 1 {
			if len(values) == 1 {
				r.params[col.Name()] = values[0]
			} else {
				r.wide[col.Name()] = values
			}
			return true, nil
		}
		alias = owners[0]
	}
	if !accepts(r.tables[alias], col.Name()) {
		return false, nil
	}
	if len(values) == 1 {
		if r.nodeParams[alias] == nil {
			r.nodeParams[alias] = map[string]string{}
		}
		r.nodeParams[alias][col.Name()] = values[0]
	} else {
		if r.fanout[alias] == nil {
			r.fanout[alias] = map[string][]string{}
		}
		r.fanout[alias][col.Name()] = values
	}
	if hasColumn(r.tables[alias], col.Name()) {
		if err := r.filter(p); err != nil {
			return false, err
		}
	}
	return true, nil
}

// bindJoin places column = column across two tables as an edge onto the side that needs the value.
// Where neither needs it the tables run independently and the equality is a filter on their rows.
func (r *resolver) bindJoin(p query.Predicate) (bool, error) {
	c, ok := p.(query.Compare)
	if !ok || c.Op() != query.Eq {
		return false, nil
	}
	l, lok := c.Left().(query.Column)
	rc, rok := c.Right().(query.Column)
	switch {
	case lok && !rok:
		return r.bindComputed(c, l, c.Right())
	case rok && !lok:
		return r.bindComputed(c, rc, c.Left())
	case !lok && !rok:
		return false, nil
	}
	var err error
	if l, err = r.qualify(l); err != nil {
		return false, fmt.Errorf("omnisdk: %s: %w", describe(c), err)
	}
	if rc, err = r.qualify(rc); err != nil {
		return false, fmt.Errorf("omnisdk: %s: %w", describe(c), err)
	}
	if l.Qualifier() == rc.Qualifier() {
		return false, nil
	}
	lNeeds := needs(r.tables[l.Qualifier()], l.Name(), r.known(l.Qualifier()))
	rNeeds := needs(r.tables[rc.Qualifier()], rc.Name(), r.known(rc.Qualifier()))
	var to, from query.Column
	switch {
	case lNeeds && rNeeds:
		return false, fmt.Errorf("omnisdk: %s: %s and %s each need the other", describe(c), l.Qualifier(), rc.Qualifier())
	case lNeeds:
		to, from = l, rc
	case rNeeds:
		to, from = rc, l
	default:
		return false, nil
	}
	if _, seen := r.arrivals[to.Qualifier()]; !seen {
		r.consumers = append(r.consumers, to.Qualifier())
	}
	r.arrivals[to.Qualifier()] = append(r.arrivals[to.Qualifier()], arrival{from: from.Qualifier(), src: kept(from.Name()), as: to.Name()})
	r.keep(from.Qualifier(), from.Name())
	if hasColumn(r.tables[to.Qualifier()], to.Name()) {
		if err := r.filter(c); err != nil {
			return false, err
		}
	}
	return true, nil
}

// bindComputed places column = f(other table's columns). Where the column's table needs the value,
// the producer computes it in its projection, before its rows travel the edge; otherwise it is a
// filter. A function on the needing side cannot be inverted, so it never binds.
func (r *resolver) bindComputed(c query.Compare, col query.Column, e query.Expr) (bool, error) {
	if _, isCall := e.(query.Call); !isCall {
		return false, nil
	}
	col, err := r.qualify(col)
	if err != nil {
		return false, fmt.Errorf("omnisdk: %s: %w", describe(c), err)
	}
	from, x, err := r.projected(e)
	if err != nil || from == col.Qualifier() {
		return false, nil
	}
	if !needs(r.tables[col.Qualifier()], col.Name(), r.known(col.Qualifier())) {
		return false, nil
	}
	name := computed(len(r.computed[from]))
	r.computed[from] = append(r.computed[from], NewSelectColumn(name, x))
	if _, seen := r.arrivals[col.Qualifier()]; !seen {
		r.consumers = append(r.consumers, col.Qualifier())
	}
	r.arrivals[col.Qualifier()] = append(r.arrivals[col.Qualifier()], arrival{from: from, src: name, as: col.Name()})
	if hasColumn(r.tables[col.Qualifier()], col.Name()) {
		if err := r.filter(c); err != nil {
			return false, err
		}
	}
	return true, nil
}

// filter adds a condition on the returned rows, with its columns qualified and pointed at the
// names their nodes keep them under.
func (r *resolver) filter(p query.Predicate) error {
	q, err := r.rewrite(p)
	if err != nil {
		return fmt.Errorf("omnisdk: %s: %w", describe(p), err)
	}
	r.filters = append(r.filters, q)
	return nil
}

func (r *resolver) rewrite(p query.Predicate) (query.Predicate, error) {
	switch p := p.(type) {
	case query.Compare:
		l, err := r.rewriteExpr(p.Left())
		if err != nil {
			return nil, err
		}
		rt, err := r.rewriteExpr(p.Right())
		if err != nil {
			return nil, err
		}
		return query.NewCompare(p.Op(), l, rt), nil
	case query.In:
		e, err := r.rewriteExpr(p.Expr())
		if err != nil {
			return nil, err
		}
		set, err := r.rewriteExpr(p.Set())
		if err != nil {
			return nil, err
		}
		return query.NewIn(e, set), nil
	case query.Test:
		e, err := r.rewriteExpr(p.Cond())
		if err != nil {
			return nil, err
		}
		return query.NewTest(e), nil
	case query.Or:
		var any []query.Predicate
		for _, q := range p.Any() {
			x, err := r.rewrite(q)
			if err != nil {
				return nil, err
			}
			any = append(any, x)
		}
		return query.NewOr(any...), nil
	case query.Not:
		x, err := r.rewrite(p.Negated())
		if err != nil {
			return nil, err
		}
		return query.NewNot(x), nil
	}
	return nil, fmt.Errorf("unsupported condition %T", p)
}

func (r *resolver) rewriteExpr(e query.Expr) (query.Expr, error) {
	switch e := e.(type) {
	case query.Column:
		c, err := r.qualify(e)
		if err != nil {
			return nil, err
		}
		r.keep(c.Qualifier(), c.Name())
		return query.NewColumn(c.Qualifier(), kept(c.Name())), nil
	case query.Collection:
		var items []query.Expr
		for _, x := range e.Items() {
			y, err := r.rewriteExpr(x)
			if err != nil {
				return nil, err
			}
			items = append(items, y)
		}
		return query.NewCollection(items...), nil
	case query.Call:
		var args []query.Expr
		for _, x := range e.Args() {
			y, err := r.rewriteExpr(x)
			if err != nil {
				return nil, err
			}
			args = append(args, y)
		}
		return query.NewCall(e.Func(), args...), nil
	}
	return e, nil
}

// keep records a column a node must carry past its projection for an edge or a filter.
func (r *resolver) keep(alias, name string) {
	if !contains(r.retain[alias], name) {
		r.retain[alias] = append(r.retain[alias], name)
	}
}

// build assembles the graph: nodes with their bound params, one projection per table carrying its
// share of the select list plus what its edges read, and the wirings.
func (r *resolver) build(sel []query.Output) (Resolution, error) {
	byAlias := map[string][]SelectColumn{}
	for _, o := range sel {
		if len(r.aliasesOf(o.Expr())) > 1 {
			// Reads several tables: computed on the finished row, from what each node keeps.
			e, err := r.rewriteExpr(o.Expr())
			if err != nil {
				return nil, fmt.Errorf("omnisdk: output %q: %w", o.Name(), err)
			}
			r.finals = append(r.finals, query.NewOutput(o.Name(), e))
			continue
		}
		alias, e, err := r.projected(o.Expr())
		if err != nil {
			return nil, fmt.Errorf("omnisdk: output %q: %w", o.Name(), err)
		}
		byAlias[alias] = append(byAlias[alias], NewSelectColumn(o.Name(), e))
	}
	var nodes []Node
	var projections []Projection
	for _, alias := range r.order {
		nodes = append(nodes, NewFanoutNode(alias, r.tables[alias].Address(), r.nodeParams[alias], r.fanout[alias]))
		cols := byAlias[alias]
		// A column an edge or a filter reads is kept whether or not the query selected it: the
		// projection replaces the row, and they read it afterwards. Kept under a hidden name, so it
		// is dropped from the output the query asked for.
		for _, name := range r.retain[alias] {
			cols = append(cols, NewSelectColumn(kept(name), NewField(name)))
		}
		cols = append(cols, r.computed[alias]...)
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
			in = append(in, NewInbound(a.from, a.src, a.as))
		}
		wirings = append(wirings, NewWiring(to, in, "", ""))
	}
	g, err := NewGraphWithFilters(nodes, wirings, projections, NewRowOps(r.filters, r.wide, r.finals))
	if err != nil {
		return nil, err
	}
	return resolution{graph: g, params: r.params}, nil
}

// projected converts an output expression to the engine's, reporting the one table it reads.
func (r *resolver) projected(e query.Expr) (string, Expression, error) {
	var alias string
	var failed error
	x, err := engineExpr(e, func(c query.Column) fn.Expr {
		q, err := r.qualify(c)
		switch {
		case err != nil:
			failed = err
		case alias != "" && alias != q.Qualifier():
			failed = fmt.Errorf("reads %s and %s; an expression across tables is not yet supported", alias, q.Qualifier())
		default:
			alias = q.Qualifier()
		}
		return fn.Field(c.Name())
	})
	switch {
	case err != nil:
		return "", nil, err
	case failed != nil:
		return "", nil, failed
	case alias == "":
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
	for name := range r.wide {
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

// computed names the n'th value a node computes for an edge; egress drops it.
func computed(n int) string { return fmt.Sprintf("\x00=%d", n) }

// aliasesOf are the tables an expression reads, where each column can be placed.
func (r *resolver) aliasesOf(e query.Expr) []string {
	var out []string
	var walk func(query.Expr)
	walk = func(e query.Expr) {
		switch e := e.(type) {
		case query.Column:
			if c, err := r.qualify(e); err == nil && !contains(out, c.Qualifier()) {
				out = append(out, c.Qualifier())
			}
		case query.Collection:
			for _, x := range e.Items() {
				walk(x)
			}
		case query.Call:
			for _, x := range e.Args() {
				walk(x)
			}
		}
	}
	walk(e)
	return out
}
