package omnisdk

import (
	"fmt"
	"maps"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/fn"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/schemaxml"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

// Node is one reference to a document-declared exchange: the address it runs, and the alias naming
// this use of it. Everything else in a graph — wirings, projections — names the alias, as SQL names a
// table reference rather than a table.
//
// It exists because an address is not an identity. A query may run the same address twice — a
// self-join, or one table read in two regions — and each reference is its own exchange, with its own
// inputs and its own rows. Keyed by address, the second reference silently merged into the first.
type Node interface {
	// Alias names this reference; unique within a graph. It is the address when none is given, as
	// an unaliased SQL table is referenced by its own name — so the same address twice needs aliases.
	Alias() string
	// Address is the exchange it runs, "<provider>.<service>.<resource>".
	Address() string
	// Params are pushdown inputs for this reference alone. They take precedence over Args.Params,
	// which is what lets two references to one address run with different values.
	Params() map[string]string
	// Fanout are inputs taking several values: the reference runs once per value, and once per
	// combination where there are several — an IN list pushed down.
	Fanout() map[string][]string
}

// NewNode declares one reference to an address. alias may be empty, meaning the address itself;
// params may be nil.
func NewNode(alias, address string, params map[string]string) Node {
	return NewFanoutNode(alias, address, params, nil)
}

// NewFanoutNode declares a reference with multi-valued inputs as well. fanout may be nil.
func NewFanoutNode(alias, address string, params map[string]string, fanout map[string][]string) Node {
	if alias == "" {
		alias = address
	}
	return node{alias: alias, address: address, params: params, fanout: fanout}
}

type node struct {
	alias, address string
	params         map[string]string
	fanout         map[string][]string
}

func (n node) Alias() string               { return n.alias }
func (n node) Address() string             { return n.address }
func (n node) Params() map[string]string   { return n.params }
func (n node) Fanout() map[string][]string { return n.fanout }

// Inbound is one value arriving at a consumer: an attribute a producer emits, landing in the
// consumer's inbox. It is a β edge stated from the consuming side, which is where a caller thinks
// about it — "this call needs the id that call produced".
//
// It exists because a document describes one provider and cannot state a relationship spanning two,
// or one its author simply did not write down.
type Inbound interface {
	// From is the producing node's alias.
	From() string
	// Src is the attribute it emits.
	Src() string
	// As is the name the value takes in the consumer's inbox; empty means Src unchanged.
	As() string
}

// NewInbound declares one arriving value.
func NewInbound(from, src, as string) Inbound {
	if as == "" {
		as = src
	}
	return inbound{from: from, src: src, as: as}
}

type inbound struct{ from, src, as string }

func (i inbound) From() string { return i.from }
func (i inbound) Src() string  { return i.src }
func (i inbound) As() string   { return i.as }

// Wiring is everything arriving at one consumer, and how its inbox becomes its inputs.
//
// Via is T_in. It belongs to the consumer rather than to any single edge because one input may be
// built from values several producers supplied — an identifier from one call wrapped into the
// filter expression another call's API actually accepts. A per-edge transform could not express
// that, and β itself carries none: it is pure value transfer.
type Wiring interface {
	// To is the consuming node's alias.
	To() string
	Inbound() []Inbound
	// Via names a DSL program mapping the inbox to the consumer's inputs, in the same language a
	// document's transforms are written in. Empty is identity: the inbox IS the input, by name.
	Via() (dslType, program string)
	// Provides names the inputs Via builds. It has to be stated because placement happens when the
	// plan is built and the program does not run until a row arrives: an optional parameter nobody
	// supplied is dropped from the request, so without this the program would produce a value with
	// nowhere to go. With no Via the inbox names itself, and this is unnecessary.
	Provides() []string
}

// NewWiring declares a consumer's inbox. via may be empty, which is the common case: a value that
// already has the shape the consumer needs passes straight through.
func NewWiring(to string, in []Inbound, viaType, viaProgram string, provides ...string) Wiring {
	return wiring{to: to, in: in, viaType: viaType, viaProgram: viaProgram, provides: provides}
}

type wiring struct {
	to                  string
	in                  []Inbound
	viaType, viaProgram string
	provides            []string
}

func (w wiring) To() string            { return w.to }
func (w wiring) Inbound() []Inbound    { return w.in }
func (w wiring) Via() (string, string) { return w.viaType, w.viaProgram }
func (w wiring) Provides() []string    { return w.provides }

// Override corrects what a document says about one exchange's response.
//
// A document can be wrong for this engine rather than wrong in itself: AWS's EC2 bundle declares a
// row path of "$.line_items", a shape produced by a schema-driven reshape that is named but not
// implemented here, so the query returns nothing and says nothing about why. Editing the bundle is
// not the remedy — a caller states what the document should have said, for this query.
type Override interface {
	// Address is the exchange being corrected.
	Address() string
	// ObjectKey replaces the document's row path; empty leaves it.
	ObjectKey() string
	// MediaType replaces what the document says the body is; empty leaves it.
	MediaType() string
	// Program replaces the document's response transform; empty type leaves it.
	Program() (dslType, body string)
}

// NewOverride corrects one exchange's response handling.
func NewOverride(address, objectKey, mediaType, programType, programBody string) Override {
	return override{address: address, objectKey: objectKey, mediaType: mediaType,
		programType: programType, programBody: programBody}
}

type override struct {
	address, objectKey, mediaType string
	programType, programBody      string
}

func (o override) Address() string           { return o.address }
func (o override) ObjectKey() string         { return o.objectKey }
func (o override) MediaType() string         { return o.mediaType }
func (o override) Program() (string, string) { return o.programType, o.programBody }

// Graph is a query over several document-declared exchanges, wired by the caller.
type Graph interface {
	// Nodes are the references taking part, in declaration order.
	Nodes() []Node
	Wirings() []Wiring
	// Overrides correct what the documents say, per address.
	Overrides() []Override
	// Projections are select lists applied per node, replacing the document's row.
	Projections() []Projection
	// Filters are conditions every returned row meets. Columns are qualified by alias and name a
	// node's row as it leaves that node — after its projection, where it has one.
	Filters() []query.Predicate
	// Fanout are query-wide inputs taking several values, e.g. region IN (...): the whole query
	// runs once per value, and every node in one run sees the same value.
	Fanout() map[string][]string
}

// NewGraph declares a multi-exchange query.
func NewGraph(nodes []Node, wirings []Wiring, overrides ...Override) (Graph, error) {
	return NewGraphWithProjections(nodes, wirings, nil, overrides...)
}

// NewGraphWithProjections declares a multi-exchange query whose nodes may carry select lists.
// A projection is applied to that node's rows BEFORE they travel an edge, so a joined-on value
// may be one a function computed rather than one the document returned.
func NewGraphWithProjections(nodes []Node, wirings []Wiring, projections []Projection, overrides ...Override) (Graph, error) {
	return NewGraphWithFilters(nodes, wirings, projections, nil, nil, overrides...)
}

// NewGraphWithFilters declares a multi-exchange query whose rows must also meet filters —
// conditions no request can apply, evaluated on each row the query returns — and which may run
// once per value of a query-wide fanout.
func NewGraphWithFilters(nodes []Node, wirings []Wiring, projections []Projection, filters []query.Predicate, fanout map[string][]string, overrides ...Override) (Graph, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("omnisdk: a graph needs at least one node")
	}
	known := make(map[string]bool, len(nodes))
	addresses := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		switch {
		case n.Alias() == "":
			return nil, fmt.Errorf("omnisdk: a node has neither an alias nor an address")
		case strings.ContainsRune(n.Alias(), 0):
			return nil, fmt.Errorf("omnisdk: alias %q contains a NUL byte", n.Alias())
		case n.Address() == "":
			return nil, fmt.Errorf("omnisdk: node %q has no address", n.Alias())
		case known[n.Alias()]:
			return nil, fmt.Errorf("omnisdk: %q is referenced twice; give each reference an alias", n.Alias())
		}
		known[n.Alias()] = true
		addresses[n.Address()] = true
	}
	wired := make(map[string]bool, len(wirings))
	for _, w := range wirings {
		// An edge naming a node the query does not run is a mistake worth catching here, where
		// it can name the alias, rather than as a missing binding at execution.
		if !known[w.To()] {
			return nil, fmt.Errorf("omnisdk: wiring targets %q, which the graph does not include", w.To())
		}
		// One consumer, one inbox: a second wiring would otherwise be ignored without a word.
		if wired[w.To()] {
			return nil, fmt.Errorf("omnisdk: %q has two wirings; state every arrival in one", w.To())
		}
		wired[w.To()] = true
		if len(w.Inbound()) == 0 {
			return nil, fmt.Errorf("omnisdk: wiring for %q declares nothing arriving", w.To())
		}
		if t, _ := w.Via(); t != "" && len(w.Provides()) == 0 {
			return nil, fmt.Errorf("omnisdk: wiring for %q shapes its inbox but names no input it provides", w.To())
		}
		for _, in := range w.Inbound() {
			if !known[in.From()] {
				return nil, fmt.Errorf("omnisdk: %s expects a value from %q, which the graph does not include", w.To(), in.From())
			}
			if in.From() == w.To() {
				return nil, fmt.Errorf("omnisdk: %s expects a value from itself; a self-join is two nodes", w.To())
			}
			if in.Src() == "" {
				return nil, fmt.Errorf("omnisdk: %s declares an arrival from %s with no source attribute", w.To(), in.From())
			}
		}
	}
	// An override corrects a document, not one use of it, so it names an address.
	for _, o := range overrides {
		if !addresses[o.Address()] {
			return nil, fmt.Errorf("omnisdk: override targets %q, which the graph does not include", o.Address())
		}
	}
	projected := make(map[string]bool, len(projections))
	for _, p := range projections {
		if !known[p.Alias()] {
			return nil, fmt.Errorf("omnisdk: projection targets %q, which the graph does not include", p.Alias())
		}
		if projected[p.Alias()] {
			return nil, fmt.Errorf("omnisdk: %q has two projections", p.Alias())
		}
		projected[p.Alias()] = true
	}
	for _, f := range filters {
		for _, c := range predicateColumns(f) {
			switch {
			case c.Qualifier() == "":
				return nil, fmt.Errorf("omnisdk: filter reads %q unqualified; name its node", c.Name())
			case !known[c.Qualifier()]:
				return nil, fmt.Errorf("omnisdk: filter reads %s.%s, which the graph does not include", c.Qualifier(), c.Name())
			}
		}
	}
	for k, vs := range fanout {
		if len(vs) == 0 {
			return nil, fmt.Errorf("omnisdk: query-wide %q has no values", k)
		}
	}
	return graph{nodes: nodes, wirings: wirings, overrides: overrides, projections: projections, filters: filters, fanout: fanout}, nil
}

type graph struct {
	nodes       []Node
	wirings     []Wiring
	overrides   []Override
	projections []Projection
	filters     []query.Predicate
	fanout      map[string][]string
}

func (g graph) Fanout() map[string][]string { return g.fanout }
func (g graph) Filters() []query.Predicate  { return g.filters }
func (g graph) Nodes() []Node               { return g.nodes }
func (g graph) Wirings() []Wiring           { return g.wirings }
func (g graph) Overrides() []Override       { return g.overrides }
func (g graph) Projections() []Projection   { return g.projections }

// NewGraphSelectQuery plans a multi-exchange query over a registry root or bundle directory.
//
// Every exchange is compiled exactly as a single-address run compiles one, so auth, signing and
// response handling are unchanged. What the caller adds is the data flow between them — the part a
// document cannot state, because it describes one provider and a relationship may span two.
func NewGraphSelectQuery(dir string, g Graph, args Args) (Plan, error) {
	if err := checkEndpoint(args); err != nil {
		return nil, err
	}
	reg, err := dsl.NewRegistry(append(gotemplate.Evaluators(), schemaxml.New())...)
	if err != nil {
		return nil, err
	}
	// "value" names the column a single-column table function emits. A select list renames it to
	// the column's own output name, so the choice only shows through where a caller uses the
	// registry directly.
	fns, err := fn.Builtins("value")
	if err != nil {
		return nil, err
	}
	inputs := docInputs(args)

	// A value arriving over an edge is not one the caller supplied, and an optional parameter nobody
	// supplied is dropped. Declaring the inbox names keeps the placeholders on the wire so the edge
	// has somewhere to bind. Where T_in reshapes the inbox, it is the transform's OUTPUT names that
	// must survive — but those are not knowable here, so an identity wiring declares its own and a
	// reshaping one relies on the consumer's required parameters.
	bound, provided, inbox := map[string][]string{}, map[string][]string{}, map[string][]string{}
	for _, w := range g.Wirings() {
		if t, _ := w.Via(); t != "" {
			// T_in reshapes the inbox, so it is the program's OUTPUT names that reach the wire. They
			// are placed but not bound: nothing emits them, they are made from the inbox.
			provided[w.To()] = append(provided[w.To()], w.Provides()...)
			for _, in := range w.Inbound() {
				inbox[w.To()] = append(inbox[w.To()], in.As())
			}
			continue
		}
		for _, in := range w.Inbound() {
			bound[w.To()] = append(bound[w.To()], in.As())
		}
	}
	// A multi-valued input arrives per row from the node's values exchange, as a wired one does.
	for _, n := range g.Nodes() {
		for k := range n.Fanout() {
			if contains(bound[n.Alias()], k) || contains(provided[n.Alias()], k) {
				return nil, fmt.Errorf("omnisdk: %s: %q is both a multi-valued input and a wired one", n.Alias(), k)
			}
			bound[n.Alias()] = append(bound[n.Alias()], k)
		}
	}

	// An edge reads its value from the node it names. The running row is one flat map every exchange
	// merges into, so two nodes emitting the same attribute — every column of a self-join — would
	// otherwise overwrite each other, and the edge would read whichever merged last. Each producer
	// therefore also writes the attributes its edges read under a key private to its alias.
	emits := map[string][]string{}
	for _, w := range g.Wirings() {
		for _, in := range w.Inbound() {
			emits[in.From()] = append(emits[in.From()], in.Src())
		}
	}
	// A filter reads the node it names by the same means.
	for _, f := range g.Filters() {
		for _, c := range predicateColumns(f) {
			if !contains(emits[c.Qualifier()], c.Name()) {
				emits[c.Qualifier()] = append(emits[c.Qualifier()], c.Name())
			}
		}
	}

	var specs []plan.ExchangeSpec
	var betas []plan.BetaEdge
	planned := make(map[string]string, len(g.Nodes()))
	taken := map[string]string{}
	// A query-wide fanout runs first and merges one value per row, so every node in that row binds
	// the same value by name. Each node compiles as though the value were supplied, which it is.
	const queryValues = "query_values"
	if fan := g.Fanout(); len(fan) > 0 {
		taken[queryValues] = ""
		specs = append(specs, valuesSpec(queryValues, "", fan))
		for k, vs := range fan {
			if _, clash := inputs[k]; clash {
				return nil, fmt.Errorf("omnisdk: %q is both a param and a query-wide multi-valued input", k)
			}
			inputs[k] = vs[0]
		}
	}
	for _, n := range g.Nodes() {
		alias, addr := n.Alias(), n.Address()
		// Two documents routinely name a method the same thing — "list" above all — and a plan names
		// its exchanges. The alias is what tells them apart; the address keeps a trace readable.
		name := planName(addr) + "__" + planName(alias)
		for _, nm := range []string{name, name + "_auth", name + "_values"} {
			if other, clash := taken[nm]; clash {
				return nil, fmt.Errorf("omnisdk: aliases %q and %q name the same plan exchange %q", other, alias, nm)
			}
			taken[nm] = alias
		}
		planned[alias] = name
		for k := range g.Fanout() {
			betas = append(betas, plan.NewBetaEdge(queryValues, name, k, k))
		}

		// This reference's own pushdown overlays the query-wide inputs. It is compiled with the
		// merged view, and bound from the seed row through a key private to the alias, so a second
		// reference to the same address can carry a different value under the same name.
		local := make(map[string]any, len(inputs)+len(n.Params()))
		for k, v := range inputs {
			local[k] = v
		}
		for k, v := range n.Params() {
			if contains(bound[alias], k) || contains(provided[alias], k) {
				return nil, fmt.Errorf("omnisdk: %s: %q is both a param and a wired input", alias, k)
			}
			if _, multi := n.Fanout()[k]; multi {
				return nil, fmt.Errorf("omnisdk: %s: %q is both a param and a multi-valued input", alias, k)
			}
			local[k] = v
			inputs[hidden(alias, k)] = v
			betas = append(betas, plan.NewBetaEdge("", name, hidden(alias, k), k))
		}
		arrivals := withArrivals(local, g, alias)
		if fan := n.Fanout(); len(fan) > 0 {
			arrivals = maps.Clone(arrivals)
			for k, vs := range fan {
				if len(vs) == 0 {
					return nil, fmt.Errorf("omnisdk: %s: %q has no values", alias, k)
				}
				arrivals[k] = "<bound>"
				betas = append(betas, plan.NewBetaEdge(name+"_values", name, hidden(alias, k), k))
			}
			specs = append(specs, valuesSpec(name+"_values", alias, fan))
		}

		c, err := openDocs(dir, addr)
		if err != nil {
			return nil, err
		}
		candidates, err := c.Operations(addr, "select")
		if err != nil {
			return nil, err
		}
		// Selection must count what ARRIVES, not only what the caller supplied. Azure's Subnets_list
		// needs a resource group and a VNet name, both of which come over the edge — judged on
		// supplied inputs alone, no select is satisfiable and the query fails before its wiring is
		// ever considered.
		ex, err := chooseExchange(candidates, arrivals)
		if err != nil {
			return nil, fmt.Errorf("omnisdk: %s (%s): %w", alias, addr, err)
		}
		var sec aot.Security
		if p := c.Provider(); p != nil {
			sec = p.Security()
		}
		opts, err := docOptions(args, sec)
		if err != nil {
			return nil, err
		}
		if sec != nil {
			opts = append(opts, docx.WithProviderSecurity(sec))
		}
		if names := bound[alias]; len(names) > 0 {
			opts = append(opts, docx.WithBound(names...))
		}
		if names := provided[alias]; len(names) > 0 {
			opts = append(opts, docx.WithProvided(names...))
		}
		if names := inbox[alias]; len(names) > 0 {
			opts = append(opts, docx.WithInbox(names...))
		}
		for _, o := range g.Overrides() {
			if o.Address() != addr {
				continue
			}
			if o.ObjectKey() != "" {
				opts = append(opts, docx.WithObjectKey(o.ObjectKey()))
			}
			if o.MediaType() != "" {
				opts = append(opts, docx.WithMediaType(o.MediaType()))
			}
			if typ, body := o.Program(); typ != "" {
				opts = append(opts, docx.WithResponseProgram(typ, body))
			}
		}
		spec, err := docx.Spec(ex, local, reg, opts...)
		if err != nil {
			return nil, fmt.Errorf("omnisdk: %s (%s): %w", alias, addr, err)
		}
		// Renaming happens BEFORE the auth edge is built, or the edge points at a name the plan no
		// longer has.
		// A document that declares service-account auth compiles to two exchanges — a token exchange
		// and the call, joined by a β edge carrying the bearer. A single-address run gets that wiring
		// for free; composing several means doing it per exchange, or the plan cannot build because
		// nothing supplies the token.
		if auth, authInputs, needs := docx.Expand(spec); needs {
			auth = docx.Rename(auth, name+"_auth")
			specs = append(specs, auth)
			betas = append(betas, plan.NewBetaEdge(auth.Name(), name, docx.TokenAttr, docx.TokenAttr))
			for k, v := range authInputs {
				inputs[k] = v
			}
		}
		spec = docx.Rename(spec, name)
		// T_in is attached LAST. Wrapping the spec hides the compiled form Expand reads, so a
		// consumer with an inbound transform would silently lose its auth exchange — and the plan
		// would refuse to build for want of a token.
		if w, ok := wiringFor(g, alias); ok {
			if typ, body := w.Via(); typ != "" {
				spec = plan.WithInbound(spec, docx.InboundProgram(reg, typ, body))
			}
		}
		// The select list is applied to this node's rows before they travel an edge, so a join
		// may be on a value a function computed. Attached after T_in for the same reason T_in is
		// attached last: wrapping Make hides the compiled form the auth expansion reads.
		if p, ok := projectionFor(g, alias); ok {
			t, err := transform.NewSelection(p.internal(), fns)
			if err != nil {
				return nil, fmt.Errorf("omnisdk: %s (%s): %w", alias, addr, err)
			}
			spec = plan.WithProject(spec, t)
		}
		// Nothing wired in means the same inputs for every upstream row: run once, replay the rest.
		if _, consumer := wiringFor(g, alias); !consumer {
			spec = replayed{ExchangeSpec: spec}
		}
		if attrs := emits[alias]; len(attrs) > 0 {
			spec = tagged{ExchangeSpec: spec, alias: alias, attrs: attrs}
		}
		specs = append(specs, spec)
	}

	for _, w := range g.Wirings() {
		for _, in := range w.Inbound() {
			betas = append(betas, plan.NewBetaEdge(planned[in.From()], planned[w.To()], hidden(in.From(), in.Src()), in.As()))
		}
	}

	return &cannedPlan{plan: plan.NewPlan(specs, betas, nil, inputs, egress(g, fns), nil), args: args}, nil
}

// egress is what every finished row passes through: the filters, then the removal of the keys
// private to each node.
func egress(g Graph, fns facade.FnRegistry) []facade.Transform {
	var out []facade.Transform
	if fs := g.Filters(); len(fs) > 0 {
		out = append(out, filterTransform{filters: fs, fns: fns})
	}
	out = append(out, unhide{})
	// Where every node states its select list, those lists are the row: the inputs that seeded it
	// are not columns the query asked for.
	if len(g.Projections()) == len(g.Nodes()) {
		keep := map[string]bool{}
		for _, p := range g.Projections() {
			for _, c := range p.Columns() {
				keep[c.Out()] = true
			}
		}
		out = append(out, onlyColumns(keep))
	}
	return out
}

// onlyColumns drops every column not named.
type onlyColumns map[string]bool

func (o onlyColumns) Apply(in facade.Page) (facade.Record, error) {
	row, ok := bind.DocMap(in)
	if !ok {
		if rec, is := in.(facade.Record); is {
			return rec, nil
		}
		return nil, fmt.Errorf("omnisdk: egress received a %T, not a record", in)
	}
	out := make(map[string]any, len(o))
	for k, v := range row {
		if o[k] {
			out[k] = v
		}
	}
	return bind.NewDocRecord(out), nil
}

// hidden is the running-row key a value takes when it belongs to one alias rather than to the row:
// a producer's emitted attribute, or a reference's own param. The NUL prefix keeps it out of any
// namespace a provider or a caller writes, and marks it for removal before a row is returned.
func hidden(alias, attr string) string { return "\x00" + alias + "\x00" + attr }

// tagged makes a producer also write the attributes its edges read under its alias's private keys.
// It wraps Flatten only, so it may sit outside any other wrapper.
type tagged struct {
	plan.ExchangeSpec
	alias string
	attrs []string
}

func (t tagged) Flatten() facade.Transform {
	inner := t.ExchangeSpec.Flatten()
	if inner == nil {
		inner = bind.NewTupleFlatten()
	}
	return tagFlatten{inner: inner, alias: t.alias, attrs: t.attrs}
}

type tagFlatten struct {
	inner facade.Transform
	alias string
	attrs []string
}

// Apply merges as the wrapped flatten does, then copies the attributes from THIS exchange's output —
// not from the merged row, where another node's value of the same name may already sit.
func (f tagFlatten) Apply(in facade.Page) (facade.Record, error) {
	rec, err := f.inner.Apply(in)
	if err != nil || rec == nil {
		return rec, err
	}
	m, ok := bind.DocMap(in)
	if !ok {
		return rec, nil
	}
	out, _ := m[bind.KeyOutput].(map[string]any)
	row, ok := bind.DocMap(rec)
	if out == nil || !ok {
		return rec, nil
	}
	merged := make(map[string]any, len(row)+len(f.attrs))
	for k, v := range row {
		merged[k] = v
	}
	for _, a := range f.attrs {
		if v, ok := out[a]; ok {
			merged[hidden(f.alias, a)] = v
		}
	}
	return bind.NewDocRecord(merged), nil
}

// unhide drops the alias-private keys from a finished row: they are plumbing, not columns.
type unhide struct{}

func (unhide) Apply(in facade.Page) (facade.Record, error) {
	row, ok := bind.DocMap(in)
	if !ok {
		if rec, is := in.(facade.Record); is {
			return rec, nil
		}
		return nil, fmt.Errorf("omnisdk: egress received a %T, not a record", in)
	}
	out := make(map[string]any, len(row))
	for k, v := range row {
		if !strings.HasPrefix(k, "\x00") {
			out[k] = v
		}
	}
	return bind.NewDocRecord(out), nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// withArrivals adds the inputs a consumer's wiring will deliver, for the purpose of choosing which
// operation to run. The values are placeholders: only the NAMES matter here, and the real values are
// bound per row. They are deliberately kept out of what the exchange is compiled with, where a
// placeholder would be sent as if it were data.
func withArrivals(inputs map[string]any, g Graph, alias string) map[string]any {
	w, ok := wiringFor(g, alias)
	if !ok {
		return inputs
	}
	out := make(map[string]any, len(inputs)+len(w.Provides()))
	for k, v := range inputs {
		out[k] = v
	}
	names := w.Provides()
	if typ, _ := w.Via(); typ == "" {
		// Identity wiring: the inbox names are the inputs.
		names = nil
		for _, in := range w.Inbound() {
			names = append(names, in.As())
		}
	}
	for _, n := range names {
		out[n] = "<bound>"
	}
	return out
}

// planName makes a string safe as part of a plan exchange name.
func planName(addr string) string {
	return strings.NewReplacer(".", "_", "-", "_").Replace(addr)
}

// wiringFor finds a consumer's declared inbox.
// projectionFor is the select list declared for alias, if any.
func projectionFor(g Graph, alias string) (projection, bool) {
	for _, p := range g.Projections() {
		if p.Alias() == alias {
			q, ok := p.(projection)
			return q, ok
		}
	}
	return projection{}, false
}

func wiringFor(g Graph, alias string) (Wiring, bool) {
	for _, w := range g.Wirings() {
		if w.To() == alias {
			return w, true
		}
	}
	return nil, false
}
