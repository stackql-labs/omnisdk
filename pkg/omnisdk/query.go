package omnisdk

import (
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// Query is a parsed SELECT before any document is consulted: the table references, the WHERE and
// ON conjuncts, and the select list. It states what SQL states and nothing more — which conjunct is
// a request input, which a join edge and which a client-side filter is decided by Analyze, from the
// documents.
type Query interface {
	Nodes() []Node
	// Filters are the WHERE and ON conjuncts. SQL does not distinguish them for an inner join, and
	// neither does analysis.
	Filters() []Expression
	// Outputs is the select list, each column qualified by alias.
	Outputs() []SelectColumn
}

// NewQuery declares a parsed query.
func NewQuery(nodes []Node, filters []Expression, outputs []SelectColumn) Query {
	return query{nodes: nodes, filters: filters, outputs: outputs}
}

type query struct {
	nodes   []Node
	filters []Expression
	outputs []SelectColumn
}

func (q query) Nodes() []Node           { return q.nodes }
func (q query) Filters() []Expression   { return q.filters }
func (q query) Outputs() []SelectColumn { return q.outputs }

// NewEquals is the predicate l = r.
func NewEquals(l, r Expression) Expression { return NewCall(opEquals, l, r) }

const opEquals = "="

// Analysis is a query resolved against documents: the graph to run and the query-wide params.
type Analysis interface {
	Graph() Graph
	// Params are the unqualified constant equalities: request inputs for every node that takes them.
	Params() map[string]string
}

type analysis struct {
	graph  Graph
	params map[string]string
}

func (a analysis) Graph() Graph              { return a.graph }
func (a analysis) Params() map[string]string { return a.params }

// MethodSignature is one select method of an exchange, as its document declares it: what it takes and
// the columns each row has. It is what a SQL front end needs to know about a table and cannot get
// from the query.
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

// Describe returns the select methods an address can run.
func Describe(dir, address string) ([]MethodSignature, error) {
	c, err := openDocs(dir, address)
	if err != nil {
		return nil, err
	}
	ops, err := c.Operations(address, "select")
	if err != nil {
		return nil, err
	}
	out := make([]MethodSignature, 0, len(ops))
	for _, ex := range ops {
		out = append(out, docSignature{ex: ex})
	}
	return out, nil
}

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

// signatures is what analysis reads of one node.
type signatures []MethodSignature

// accepts reports whether any method takes the parameter.
func (s signatures) accepts(name string) bool {
	for _, m := range s {
		for _, p := range m.Params() {
			if p.Name() == name {
				return true
			}
		}
	}
	return false
}

// needs reports whether the node cannot run on known alone and name would help: no method is
// satisfiable from known, and some method takes name.
func (s signatures) needs(name string, known map[string]bool) bool {
	if !s.accepts(name) {
		return false
	}
	for _, m := range s {
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

// Analyze resolves a query into a graph, given each node's signatures keyed by alias. Everything it
// cannot place is an error naming the conjunct or column, never a guess.
func Analyze(q Query, described map[string][]MethodSignature) (Analysis, error) {
	sigs := make(map[string]signatures, len(q.Nodes()))
	for _, n := range q.Nodes() {
		d, ok := described[n.Alias()]
		if !ok {
			return nil, fmt.Errorf("omnisdk: no signatures for %q", n.Alias())
		}
		sigs[n.Alias()] = d
	}

	// Constants first: they decide what each node can run without the others.
	params := map[string]string{}
	nodeParams := map[string]map[string]string{}
	var joins []expression
	for _, f := range q.Filters() {
		l, r, ok := equality(f)
		if !ok {
			return nil, fmt.Errorf("omnisdk: filter %s: only equalities are supported; client-side filtering is not yet", describe(f))
		}
		col, lit, ok := columnAndLiteral(l, r)
		if !ok {
			joins = append(joins, f.(expression))
			continue
		}
		v := fmt.Sprint(lit.lit)
		if col.alias == "" {
			params[col.name] = v
			continue
		}
		sig, known := sigs[col.alias]
		switch {
		case !known:
			return nil, fmt.Errorf("omnisdk: filter %s names %q, which the query does not reference", describe(f), col.alias)
		case !sig.accepts(col.name):
			return nil, fmt.Errorf("omnisdk: filter %s: %s takes no %q; client-side filtering is not yet", describe(f), col.alias, col.name)
		}
		if nodeParams[col.alias] == nil {
			nodeParams[col.alias] = map[string]string{}
		}
		nodeParams[col.alias][col.name] = v
	}
	knownFor := func(alias string) map[string]bool {
		k := map[string]bool{}
		for name := range params {
			k[name] = true
		}
		for name := range nodeParams[alias] {
			k[name] = true
		}
		return k
	}

	// An equality between two nodes is an edge onto the side that cannot run without it.
	type arrival struct{ from, src, as string }
	arrivals := map[string][]arrival{}
	var consumers []string
	retain := map[string][]string{} // columns a producer must keep for its edges
	for _, f := range joins {
		l, r, _ := equality(f)
		to, param, value, err := direct(l, r, sigs, knownFor)
		if err != nil {
			return nil, fmt.Errorf("omnisdk: filter %s: %w", describe(f), err)
		}
		src, ok := value.(expression)
		if !ok || src.alias == "" || src.call != "" {
			return nil, fmt.Errorf("omnisdk: filter %s: a computed join value is not yet supported", describe(f))
		}
		if _, seen := arrivals[to]; !seen {
			consumers = append(consumers, to)
		}
		arrivals[to] = append(arrivals[to], arrival{from: src.alias, src: src.name, as: param})
		retain[src.alias] = append(retain[src.alias], src.name)
	}

	// The select list splits by node; each column reads exactly one.
	byNode := map[string][]SelectColumn{}
	outNames := map[string]bool{}
	for _, c := range q.Outputs() {
		deps := c.Expr().Deps()
		switch {
		case len(deps) == 0:
			return nil, fmt.Errorf("omnisdk: output %q reads no node; qualify its columns", c.Out())
		case len(deps) > 1:
			return nil, fmt.Errorf("omnisdk: output %q reads %v; an expression across nodes is not yet supported", c.Out(), deps)
		case outNames[c.Out()]:
			return nil, fmt.Errorf("omnisdk: output %q is named twice; alias one of them", c.Out())
		}
		outNames[c.Out()] = true
		byNode[deps[0]] = append(byNode[deps[0]], c)
	}

	var nodes []Node
	var projections []Projection
	for _, n := range q.Nodes() {
		nodes = append(nodes, NewNode(n.Alias(), n.Address(), merged(n.Params(), nodeParams[n.Alias()])))
		cols := byNode[n.Alias()]
		// A column an edge reads is kept even where the query never selected it: the projection
		// replaces the row, and the edge reads it afterwards. Kept under a hidden name, so it is
		// dropped from the output the query asked for.
		for _, name := range retain[n.Alias()] {
			if !selects(cols, name) {
				cols = append(cols, NewSelectColumn(kept(name), NewColumn(n.Alias(), name)))
			}
		}
		if len(cols) == 0 {
			continue
		}
		p, err := NewProjection(n.Alias(), cols)
		if err != nil {
			return nil, err
		}
		projections = append(projections, p)
	}
	var wirings []Wiring
	for _, to := range consumers {
		var in []Inbound
		for _, a := range arrivals[to] {
			src := a.src
			if !selects(byNode[a.from], src) {
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
	return analysis{graph: g, params: params}, nil
}

// direct decides which side of a two-node equality consumes the other. The consumer is the side that
// cannot run without the value; both is a cycle and neither is a client-side join, and each is an
// error rather than a choice.
func direct(l, r Expression, sigs map[string]signatures, knownFor func(string) map[string]bool) (to, param string, value Expression, err error) {
	lc, lok := l.(expression)
	rc, rok := r.(expression)
	if !lok || !rok {
		return "", "", nil, fmt.Errorf("unrecognised expression")
	}
	lNeeds := lc.alias != "" && lc.call == "" && len(r.Deps()) > 0 && !contains(r.Deps(), lc.alias) &&
		sigs[lc.alias].needs(lc.name, knownFor(lc.alias))
	rNeeds := rc.alias != "" && rc.call == "" && len(l.Deps()) > 0 && !contains(l.Deps(), rc.alias) &&
		sigs[rc.alias].needs(rc.name, knownFor(rc.alias))
	switch {
	case lNeeds && rNeeds:
		return "", "", nil, fmt.Errorf("%s and %s each need the other", lc.alias, rc.alias)
	case lNeeds:
		return lc.alias, lc.name, r, nil
	case rNeeds:
		return rc.alias, rc.name, l, nil
	}
	return "", "", nil, fmt.Errorf("neither side needs the other's value; a client-side join is not yet supported")
}

func equality(f Expression) (Expression, Expression, bool) {
	x, ok := f.(expression)
	if !ok || x.call != opEquals || len(x.args) != 2 {
		return nil, nil, false
	}
	return x.args[0], x.args[1], true
}

func columnAndLiteral(l, r Expression) (col, lit expression, ok bool) {
	a, _ := l.(expression)
	b, _ := r.(expression)
	isCol := func(x expression) bool { return x.name != "" && x.call == "" }
	isLit := func(x expression) bool { return x.name == "" && x.call == "" }
	switch {
	case isCol(a) && isLit(b):
		return a, b, true
	case isCol(b) && isLit(a):
		return b, a, true
	}
	return expression{}, expression{}, false
}

func describe(e Expression) string {
	x, ok := e.(expression)
	switch {
	case !ok:
		return "?"
	case x.call != "":
		var args []string
		for _, a := range x.args {
			args = append(args, describe(a))
		}
		if x.call == opEquals && len(args) == 2 {
			return args[0] + " = " + args[1]
		}
		return fmt.Sprintf("%s(%v)", x.call, args)
	case x.alias != "":
		return x.alias + "." + x.name
	case x.name != "":
		return x.name
	}
	return fmt.Sprintf("%q", fmt.Sprint(x.lit))
}

// merged is a overlaid by b.
func merged(a, b map[string]string) map[string]string {
	if len(a)+len(b) == 0 {
		return nil
	}
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
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
