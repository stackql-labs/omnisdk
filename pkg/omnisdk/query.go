package omnisdk

import (
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/fn"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/namespace"
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
	// TakesBody reports whether the method sends a request body. Its fields are the caller's to
	// name: a document states a body's encoding, not its fields.
	TakesBody() bool
}

// ParamSignature is one declared input.
type ParamSignature interface {
	Name() string
	Required() bool
	// In is where it goes on the wire: query, path, header or body.
	In() string
}

// DescribeTable reads what the documents under dir say about an address's reads.
func DescribeTable(dir, address string) (Table, error) { return describeTable(dir, address, "select") }

// DescribeMutation reads what the documents under dir say about an address's methods for a
// mutating verb: "insert", "update" or "delete".
func DescribeMutation(dir, address, verb string) (Table, error) {
	switch verb {
	case "insert", "update", "delete":
		return describeTable(dir, address, verb)
	}
	return nil, fmt.Errorf("omnisdk: %q is not a mutating verb", verb)
}

func describeTable(dir, address, verb string) (Table, error) {
	c, err := openDocs(dir, address)
	if err != nil {
		return nil, err
	}
	ops, err := c.Operations(address, verb)
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

func (s docSignature) Method() string  { return s.ex.Name() }
func (s docSignature) TakesBody() bool { return s.ex.Request().BodyMediaType() != "" }

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
		computed: map[string][]SelectColumn{}, body: map[string]any{},
		outer: map[string][]query.Predicate{}, on: map[string][]query.Predicate{}}
	var conjuncts []query.Predicate
	for _, j := range q.From() {
		alias := j.Resource().Alias()
		t, ok := tables[alias]
		if !ok {
			return nil, fmt.Errorf("omnisdk: no table described for %q", alias)
		}
		r.tables[alias] = t
		r.order = append(r.order, alias)
		if j.Form() == query.Left {
			// A left join's ON decides what matches, not what survives: it is placed against the
			// joined node, after the rest.
			r.outer[alias] = j.On()
			continue
		}
		// Under an inner join ON and WHERE are the same filter.
		conjuncts = append(conjuncts, j.On()...)
	}
	if t := q.Target(); t != nil {
		if err := r.target(t, tables); err != nil {
			return nil, err
		}
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
	for _, alias := range r.order {
		if on, left := r.outer[alias]; left {
			if err := r.leftJoin(alias, on); err != nil {
				return nil, err
			}
		}
	}
	if r.mutating != "" && len(q.From()) > 0 {
		if _, fed := r.arrivals[r.mutating]; !fed {
			return nil, fmt.Errorf("omnisdk: nothing from the sources reaches %s; the effect would repeat once per source row", r.mutating)
		}
	}
	sel, err := r.expand(q.Select())
	if err != nil {
		return nil, err
	}
	return r.build(sel)
}

// target places a mutation's resource as the last node, running its verb's methods, and binds
// what it sets: a literal is a parameter, a source's column or a function of one is an edge.
func (r *resolver) target(t query.Target, tables map[string]Table) error {
	alias := t.Resource().Alias()
	tbl, ok := tables[alias]
	if !ok {
		return fmt.Errorf("omnisdk: no table described for %q", alias)
	}
	r.tables[alias] = tbl
	r.order = append(r.order, alias)
	r.mutating, r.verb = alias, t.Verb().String()
	ns, err := requestNamespace(tbl)
	if err != nil {
		return fmt.Errorf("omnisdk: %s %s: %w", r.verb, alias, err)
	}
	for _, a := range t.Set() {
		name, inBody, err := placeAssignment(ns, tbl, a.Column())
		if err != nil {
			return fmt.Errorf("omnisdk: %s %s: %w", r.verb, alias, err)
		}
		a = query.NewAssignment(name, a.Value())
		if inBody {
			// A body constant keeps its type; any other value arrives and is bound by name.
			var constant any
			if lit, ok := a.Value().(query.Literal); ok {
				constant = lit.Value()
			}
			r.body[name] = constant
			if constant != nil {
				continue
			}
		}
		switch v := a.Value().(type) {
		case query.Literal:
			if r.nodeParams[alias] == nil {
				r.nodeParams[alias] = map[string]string{}
			}
			r.nodeParams[alias][a.Column()] = fmt.Sprint(v.Value())
		case query.Column:
			c, err := r.qualify(v)
			if err != nil {
				return fmt.Errorf("omnisdk: %s = …: %w", a.Column(), err)
			}
			if c.Qualifier() == alias {
				return fmt.Errorf("omnisdk: %s = %s reads the row being written", a.Column(), describeExpr(v))
			}
			r.arrive(alias, arrival{from: c.Qualifier(), src: kept(c.Name()), as: a.Column()})
			r.keep(c.Qualifier(), c.Name())
		case query.Call:
			from, x, err := r.projected(v)
			if err != nil || from == alias {
				return fmt.Errorf("omnisdk: %s = %s: a value must read exactly one source", a.Column(), describeExpr(v))
			}
			name := computed(len(r.computed[from]))
			r.computed[from] = append(r.computed[from], NewSelectColumn(name, x))
			r.arrive(alias, arrival{from: from, src: name, as: a.Column()})
		default:
			return fmt.Errorf("omnisdk: %s = %s cannot be set", a.Column(), describeExpr(a.Value()))
		}
	}
	return nil
}

// requestNamespace is the vocabulary a mutation writes into: every parameter its methods declare,
// in the space the document puts it. A name two spaces claim is ambiguous and must be qualified.
func requestNamespace(t Table) (namespace.Namespace, error) {
	seen := map[string]bool{}
	var attrs []namespace.Attribute
	for _, m := range t.Methods() {
		for _, p := range m.Params() {
			a := namespace.New(namespace.Space(p.In()), p.Name())
			if !seen[a.Qualified()] {
				seen[a.Qualified()] = true
				attrs = append(attrs, a)
			}
		}
	}
	return namespace.Build(attrs)
}

// placeAssignment resolves the column an assignment writes: a declared parameter, or a field of the
// request body. The namespace is built ambiguity-free, so a name it cannot resolve is one no
// parameter declares; that is a body field where the method takes a body, and has nowhere to go
// where it does not. It returns the name as the wire knows it.
func placeAssignment(ns namespace.Namespace, t Table, column string) (string, bool, error) {
	if a, err := ns.Resolve(column); err == nil {
		return a.Name(), a.Space() == namespace.Body, nil
	}
	name := strings.TrimPrefix(column, string(namespace.Body)+".")
	for _, m := range t.Methods() {
		if m.TakesBody() {
			return name, true, nil
		}
	}
	return "", false, fmt.Errorf("%q is not a parameter, and the method takes no request body", column)
}

// arrive records a value wired into a node.
func (r *resolver) arrive(to string, a arrival) {
	if _, seen := r.arrivals[to]; !seen {
		r.consumers = append(r.consumers, to)
	}
	r.arrivals[to] = append(r.arrivals[to], a)
}

// expand replaces each star with the columns its tables' documents declare, named by column. A
// table with no declared schema cannot be expanded, and two expanded columns with one name are
// ambiguous: each is an error rather than a guess at what the query meant.
func (r *resolver) expand(sel []query.Output) ([]query.Output, error) {
	var out []query.Output
	names := map[string]string{}
	add := func(o query.Output, from string) error {
		if prev, clash := names[o.Name()]; clash {
			return fmt.Errorf("omnisdk: output %q comes from %s and %s; alias one of them", o.Name(), prev, from)
		}
		names[o.Name()] = from
		out = append(out, o)
		return nil
	}
	for _, o := range sel {
		st, ok := o.Expr().(query.Star)
		if !ok {
			if err := add(o, "the select list"); err != nil {
				return nil, err
			}
			continue
		}
		aliases := r.order
		if st.Qualifier() != "" {
			aliases = []string{st.Qualifier()}
		}
		for _, alias := range aliases {
			cols := columnsOf(r.tables[alias])
			if len(cols) == 0 {
				return nil, fmt.Errorf("omnisdk: %s.*: the document declares no columns for %s", alias, r.tables[alias].Address())
			}
			for _, c := range cols {
				if err := add(query.NewOutput(c, query.NewColumn(alias, c)), alias+".*"); err != nil {
					return nil, err
				}
			}
		}
	}
	return out, nil
}

// columnsOf are the columns any of a table's methods declares, in first-seen order.
func columnsOf(t Table) []string {
	var out []string
	for _, m := range t.Methods() {
		for _, c := range m.Columns() {
			if !contains(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
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
	// mutating is the target's alias in a mutation, and verb what it does; empty for a read.
	mutating, verb string
	// body is the target's request-body fields, see Node.Body.
	body map[string]any
	// outer is each left-joined node's ON; on is the match conditions placed against it, and
	// onTarget the node whose ON is being placed.
	outer    map[string][]query.Predicate
	on       map[string][]query.Predicate
	onTarget string
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
	if r.mutating == "" && hasColumn(r.tables[alias], col.Name()) {
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
	if r.onTarget != "" && to.Qualifier() != r.onTarget {
		return false, fmt.Errorf("omnisdk: %s: in a left join the preserved side %s cannot need a value from %s", describe(c), to.Qualifier(), r.onTarget)
	}
	if from.Qualifier() == r.mutating {
		return false, fmt.Errorf("omnisdk: %s: %s would need a value the %s returns, after its effect", describe(c), to.Qualifier(), r.verb)
	}
	if _, seen := r.arrivals[to.Qualifier()]; !seen {
		r.consumers = append(r.consumers, to.Qualifier())
	}
	r.arrivals[to.Qualifier()] = append(r.arrivals[to.Qualifier()], arrival{from: from.Qualifier(), src: kept(from.Name()), as: to.Name()})
	r.keep(from.Qualifier(), from.Name())
	if r.mutating == "" && hasColumn(r.tables[to.Qualifier()], to.Name()) {
		if err := r.filter(c); err != nil {
			return false, err
		}
	}
	return true, nil
}

// leftJoin places a left join's ON against its node. A constant its methods take is pushed down, an
// equality it needs is an edge into it, and anything else is a match condition on its rows: all of
// them narrow what matches, and none drops an upstream row. The preserved side needing a value from
// the joined one is refused — it would have no rows to preserve.
func (r *resolver) leftJoin(alias string, on []query.Predicate) error {
	r.onTarget = alias
	defer func() { r.onTarget = "" }()
	var rest []query.Predicate
	for _, p := range on {
		placed, err := r.bindConstant(p)
		if err != nil {
			return err
		}
		if !placed {
			rest = append(rest, p)
		}
	}
	for _, p := range rest {
		placed, err := r.bindJoin(p)
		if err != nil {
			return err
		}
		if !placed {
			if err := r.filter(p); err != nil {
				return err
			}
		}
	}
	return nil
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
	if r.onTarget != "" && col.Qualifier() != r.onTarget {
		return false, fmt.Errorf("omnisdk: %s: in a left join the preserved side %s cannot need a value from %s", describe(c), col.Qualifier(), r.onTarget)
	}
	name := computed(len(r.computed[from]))
	r.computed[from] = append(r.computed[from], NewSelectColumn(name, x))
	if _, seen := r.arrivals[col.Qualifier()]; !seen {
		r.consumers = append(r.consumers, col.Qualifier())
	}
	r.arrivals[col.Qualifier()] = append(r.arrivals[col.Qualifier()], arrival{from: from, src: name, as: col.Name()})
	if r.mutating == "" && hasColumn(r.tables[col.Qualifier()], col.Name()) {
		if err := r.filter(c); err != nil {
			return false, err
		}
	}
	return true, nil
}

// filter adds a condition on the returned rows, with its columns qualified and pointed at the
// names their nodes keep them under.
func (r *resolver) filter(p query.Predicate) error {
	if r.mutating != "" {
		return fmt.Errorf("omnisdk: %s: in a mutation this could only be checked on rows, after the effect; it must be a parameter the methods take", describe(p))
	}
	q, err := r.rewrite(p)
	if err != nil {
		return fmt.Errorf("omnisdk: %s: %w", describe(p), err)
	}
	if r.onTarget == "" {
		r.filters = append(r.filters, q)
		return nil
	}
	// A match condition runs on the joined node's rows. Another node's column reaches it through
	// the inbox, under that node's private key, and is never sent.
	for _, c := range predicateColumns(q) {
		if c.Qualifier() == r.onTarget {
			continue
		}
		as := hidden(c.Qualifier(), c.Name())
		if !r.arriving(r.onTarget, as) {
			r.arrive(r.onTarget, arrival{from: c.Qualifier(), src: c.Name(), as: as})
		}
	}
	r.on[r.onTarget] = append(r.on[r.onTarget], q)
	return nil
}

// arriving reports whether a value already arrives at a node under a name.
func (r *resolver) arriving(to, as string) bool {
	for _, a := range r.arrivals[to] {
		if a.as == as {
			return true
		}
	}
	return false
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
		verb := "select"
		if alias == r.mutating {
			verb = r.verb
		}
		var body map[string]any
		if alias == r.mutating {
			body = r.body
		}
		n := NewMutationNode(alias, r.tables[alias].Address(), verb, r.nodeParams[alias], r.fanout[alias], body)
		if _, left := r.outer[alias]; left {
			n = NewOuterNode(n, r.on[alias])
		}
		nodes = append(nodes, n)
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
