package omnisdk

import "time"

// Names kept for callers built against earlier releases. Each is the current function under its old
// name, with the same signature and behaviour.

// NewFromCatalog is NewSelectFromCatalog.
//
// Deprecated: use NewSelectFromCatalog.
func NewFromCatalog(dir, address string, args Args) (Plan, error) {
	return NewSelectFromCatalog(dir, address, args)
}

// NewGraphSelectQuery is NewGraphQuery. A graph was never limited to reads.
//
// Deprecated: use NewGraphQuery.
func NewGraphSelectQuery(dir string, g Graph, args Args) (Plan, error) {
	return NewGraphQuery(dir, g, args)
}

// NodesOf references each address once, aliased by the address itself — which is how a graph
// declared by addresses read. Wirings and projections naming those addresses resolve unchanged.
// An address listed twice is refused by NewGraph: two references need two aliases.
func NodesOf(addresses ...string) []Node {
	out := make([]Node, 0, len(addresses))
	for _, a := range addresses {
		out = append(out, NewNode("", a, nil))
	}
	return out
}

// Addresses are the addresses a graph's nodes run, in node order.
//
// Deprecated: use Nodes; an address may appear under several aliases.
func (g graph) Addresses() []string {
	out := make([]string, 0, len(g.nodes))
	for _, n := range g.nodes {
		out = append(out, n.Address())
	}
	return out
}

// Converge is Prepare with DesiredState under NewLedger, as callers built against earlier releases
// call it. An empty runID names the run's journal by the time, as it always did.
//
// Deprecated: use Prepare(registry, DesiredState(resources), NewLedger(name, state, runID), args).
func Converge(registry, name, state, runID string, resources []ManagedResource, args Args) (Plan, error) {
	if runID == "" {
		runID = time.Now().UTC().Format("20060102T150405Z")
	}
	// Converge always kept its own journal and ignored a query's.
	args.Journal = nil
	return Prepare(registry, DesiredState(resources), NewLedger(name, state, runID), args)
}
