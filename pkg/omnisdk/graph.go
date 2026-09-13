package omnisdk

import (
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/schemaxml"
)

// Inbound is one value arriving at a consumer: an attribute a producer emits, landing in the
// consumer's inbox. It is a β edge stated from the consuming side, which is where a caller thinks
// about it — "this call needs the id that call produced".
//
// It exists because a document describes one provider and cannot state a relationship spanning two,
// or one its author simply did not write down.
type Inbound interface {
	// From is the producing exchange's address.
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
	// To is the consuming exchange's address.
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
	// Addresses are the exchanges taking part, each "<provider>.<service>.<resource>".
	Addresses() []string
	Wirings() []Wiring
	// Overrides correct what the documents say, per address.
	Overrides() []Override
}

// NewGraph declares a multi-exchange query.
func NewGraph(addresses []string, wirings []Wiring, overrides ...Override) (Graph, error) {
	if len(addresses) == 0 {
		return nil, fmt.Errorf("omnisdk: a graph needs at least one address")
	}
	known := make(map[string]bool, len(addresses))
	for _, a := range addresses {
		known[a] = true
	}
	for _, w := range wirings {
		// An edge naming an exchange the query does not run is a mistake worth catching here, where
		// it can name the address, rather than as a missing binding at execution.
		if !known[w.To()] {
			return nil, fmt.Errorf("omnisdk: wiring targets %q, which the graph does not include", w.To())
		}
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
			if in.Src() == "" {
				return nil, fmt.Errorf("omnisdk: %s declares an arrival from %s with no source attribute", w.To(), in.From())
			}
		}
	}
	for _, o := range overrides {
		if !known[o.Address()] {
			return nil, fmt.Errorf("omnisdk: override targets %q, which the graph does not include", o.Address())
		}
	}
	return graph{addresses: addresses, wirings: wirings, overrides: overrides}, nil
}

type graph struct {
	addresses []string
	wirings   []Wiring
	overrides []Override
}

func (g graph) Addresses() []string   { return g.addresses }
func (g graph) Wirings() []Wiring     { return g.wirings }
func (g graph) Overrides() []Override { return g.overrides }

// resolved pairs an address with the name its exchange takes inside the plan. A plan names an
// exchange by the document's own method name, so a join stated in addresses must be translated
// before it can become an edge.
type resolved struct {
	planned string
}

// NewGraphQuery plans a multi-exchange query over a registry root or bundle directory.
//
// Every exchange is compiled exactly as a single-address run compiles one, so auth, signing and
// response handling are unchanged. What the caller adds is the data flow between them — the part a
// document cannot state, because it describes one provider and a relationship may span two.
func NewGraphQuery(dir string, g Graph, args Args) (Plan, error) {
	if err := checkEndpoint(args); err != nil {
		return nil, err
	}
	reg, err := dsl.NewRegistry(append(gotemplate.Evaluators(), schemaxml.New())...)
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

	var specs []plan.ExchangeSpec
	var betas []plan.BetaEdge
	byAddress := make(map[string]resolved, len(g.Addresses()))
	for _, addr := range g.Addresses() {
		c, err := openDocs(dir, addr)
		if err != nil {
			return nil, err
		}
		candidates, err := c.Exchanges(addr)
		if err != nil {
			return nil, err
		}
		// Selection must count what ARRIVES, not only what the caller supplied. Azure's Subnets_list
		// needs a resource group and a VNet name, both of which come over the edge — judged on
		// supplied inputs alone, no select is satisfiable and the query fails before its wiring is
		// ever considered.
		ex, err := chooseExchange(candidates, withArrivals(inputs, g, addr))
		if err != nil {
			return nil, fmt.Errorf("omnisdk: %s: %w", addr, err)
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
		if names := bound[addr]; len(names) > 0 {
			opts = append(opts, docx.WithBound(names...))
		}
		if names := provided[addr]; len(names) > 0 {
			opts = append(opts, docx.WithProvided(names...))
		}
		if names := inbox[addr]; len(names) > 0 {
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
		spec, err := docx.Spec(ex, inputs, reg, opts...)
		if err != nil {
			return nil, fmt.Errorf("omnisdk: %s: %w", addr, err)
		}
		// Two documents routinely name a method the same thing — "list" above all — and a plan names
		// its exchanges. The address is what tells them apart. Renaming happens BEFORE the auth edge
		// is built, or the edge points at a name the plan no longer has.
		name := planName(addr)
		// A document that declares service-account auth compiles to two exchanges — a token exchange
		// and the call, joined by a β edge carrying the bearer. A single-address run gets that wiring
		// for free; composing several means doing it per exchange, or the plan cannot build because
		// nothing supplies the token.
		if auth, assertion, needs := docx.Expand(spec); needs {
			auth = docx.Rename(auth, name+"_auth")
			specs = append(specs, auth)
			betas = append(betas, plan.NewBetaEdge(auth.Name(), name, docx.TokenAttr, docx.TokenAttr))
			inputs["assertion"] = assertion
		}
		spec = docx.Rename(spec, name)
		// T_in is attached LAST. Wrapping the spec hides the compiled form Expand reads, so a
		// consumer with an inbound transform would silently lose its auth exchange — and the plan
		// would refuse to build for want of a token.
		if w, ok := wiringFor(g, addr); ok {
			if typ, body := w.Via(); typ != "" {
				spec = plan.WithInbound(spec, docx.InboundProgram(reg, typ, body))
			}
		}
		byAddress[addr] = resolved{planned: spec.Name()}
		specs = append(specs, spec)
	}

	for _, w := range g.Wirings() {
		for _, in := range w.Inbound() {
			betas = append(betas, plan.NewBetaEdge(byAddress[in.From()].planned, byAddress[w.To()].planned, in.Src(), in.As()))
		}
	}

	return &cannedPlan{plan: plan.NewPlan(specs, betas, nil, inputs, nil, nil), args: args}, nil
}

// withArrivals adds the inputs a consumer's wiring will deliver, for the purpose of choosing which
// operation to run. The values are placeholders: only the NAMES matter here, and the real values are
// bound per row. They are deliberately kept out of what the exchange is compiled with, where a
// placeholder would be sent as if it were data.
func withArrivals(inputs map[string]any, g Graph, addr string) map[string]any {
	w, ok := wiringFor(g, addr)
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

// planName is the name an address takes inside the plan: distinct per address, and readable in a
// trace.
func planName(addr string) string {
	return strings.NewReplacer(".", "_", "-", "_").Replace(addr)
}

// wiringFor finds a consumer's declared inbox.
func wiringFor(g Graph, addr string) (Wiring, bool) {
	for _, w := range g.Wirings() {
		if w.To() == addr {
			return w, true
		}
	}
	return nil, false
}
