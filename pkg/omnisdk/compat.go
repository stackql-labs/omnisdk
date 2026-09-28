package omnisdk

// Names kept for callers built against earlier releases. Each is the current function under its old
// name, with the same signature and behaviour.

// NewFromCatalog is NewSelectFromCatalog.
//
// Deprecated: use NewSelectFromCatalog.
func NewFromCatalog(dir, address string, args Args) (Plan, error) {
	return NewSelectFromCatalog(dir, address, args)
}

// NewGraphQuery is NewGraphSelectQuery.
//
// Deprecated: use NewGraphSelectQuery.
func NewGraphQuery(dir string, g Graph, args Args) (Plan, error) {
	return NewGraphSelectQuery(dir, g, args)
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
