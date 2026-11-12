package omnisdk

import (
	"fmt"
	"maps"
	"slices"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

// InboundCandidate is an alternative source, not an execution dependency.
// Canonical candidates rank before noncanonical ones, then by ascending Cost.
// Equal scores retain declaration order. Costs are local estimates, not sums.
type InboundCandidate struct {
	From      string `json:"from" yaml:"from"`
	Src       string `json:"src" yaml:"src"`
	As        string `json:"as,omitempty" yaml:"as,omitempty"`
	Canonical bool   `json:"canonical,omitempty" yaml:"canonical,omitempty"`
	Cost      int    `json:"cost,omitempty" yaml:"cost,omitempty"`
}

func wiringRequired(w Wiring) []string {
	if metadata, ok := w.(interface{ Required() []string }); ok {
		return metadata.Required()
	}
	return nil
}
func wiringCandidates(w Wiring) []InboundCandidate {
	if metadata, ok := w.(interface{ Candidates() []InboundCandidate }); ok {
		return metadata.Candidates()
	}
	return nil
}
func copyWiring(w Wiring, in []Inbound) wiring {
	typ, body := w.Via()
	copied := wiring{to: w.To(), in: slices.Clone(in), viaType: typ, viaProgram: body, provides: slices.Clone(w.Provides()), required: wiringRequired(w), candidates: wiringCandidates(w)}
	if metadata, ok := w.(interface{ Admission() *dsl.Program }); ok {
		copied.admission = metadata.Admission()
	}
	return copied
}

// WithInboundCandidates adds potential defaults without changing explicit edges.
func WithInboundCandidates(w Wiring, candidates ...InboundCandidate) Wiring {
	copied := copyWiring(w, w.Inbound())
	copied.candidates = append(copied.candidates, candidates...)
	return copied
}

type canonicalGraph struct {
	Graph
	nodes        []Node
	wirings      []Wiring
	terminations map[string]Termination
	projections  []Projection
}

func (g canonicalGraph) Projections() []Projection            { return g.projections }
func (g canonicalGraph) Nodes() []Node                        { return g.nodes }
func (g canonicalGraph) Wirings() []Wiring                    { return g.wirings }
func (g canonicalGraph) Terminations() map[string]Termination { return g.terminations }
func (g canonicalGraph) Addresses() []string {
	out := make([]string, len(g.nodes))
	for i, node := range g.nodes {
		out[i] = node.Address()
	}
	return out
}

// selectCanonicalInputs is a greedy, deterministic per-input selector. It does
// not enumerate subsets, execute observations, or backtrack over earlier choices.
func selectCanonicalInputs(g Graph, args Args) (Graph, error) {
	enabled := false
	for _, w := range g.Wirings() {
		if len(wiringRequired(w)) > 0 || len(wiringCandidates(w)) > 0 {
			enabled = true
			break
		}
	}
	if !enabled {
		return g, nil
	}
	nodes := map[string]Node{}
	wirings := map[string]wiring{}
	potential := map[string]bool{}
	for _, node := range g.Nodes() {
		nodes[node.Alias()] = node
	}
	for _, w := range g.Wirings() {
		copied := copyWiring(w, w.Inbound())
		seen := map[string]bool{}
		for _, name := range copied.required {
			if name == "" || seen[name] {
				return nil, fmt.Errorf("%s: required inputs must be nonempty and unique", w.To())
			}
			seen[name] = true
		}
		for i, candidate := range copied.candidates {
			if candidate.As == "" {
				candidate.As = candidate.Src
				copied.candidates[i] = candidate
			}
			if candidate.Cost < 0 || candidate.From == "" || candidate.Src == "" {
				return nil, fmt.Errorf("%s: candidate needs from/src and nonnegative cost", w.To())
			}
			if source, exists := nodes[candidate.From]; exists {
				if source.Verb() != "select" {
					return nil, fmt.Errorf("%s: candidate %s must be a read; effects require explicit wiring", w.To(), candidate.From)
				}
				potential[candidate.From] = true
			}
		}
		wirings[w.To()] = copied
	}
	// Candidate-only upstream reads are pruned unless selected or explicitly used.
	var markPotential func(string)
	markPotential = func(alias string) {
		if w, ok := wirings[alias]; ok {
			for _, edge := range w.in {
				if node := nodes[edge.From()]; node != nil && node.Verb() == "select" && !potential[edge.From()] {
					potential[edge.From()] = true
					markPotential(edge.From())
				}
			}
		}
	}
	for _, alias := range slices.Sorted(maps.Keys(potential)) {
		markPotential(alias)
	}
	roots := map[string]bool{}
	for alias := range nodes {
		if !potential[alias] {
			roots[alias] = true
		}
	}

	retainColumns := func(predicate query.Predicate) {
		for _, column := range predicateColumns(predicate) {
			roots[column.Qualifier()] = true
		}
	}
	for _, filter := range g.Filters() {
		retainColumns(filter)
	}
	for _, output := range g.Outputs() {
		retainColumns(query.NewTest(output.Expr()))
	}
	for _, branch := range g.Branches() {
		for _, arm := range branch.Arms() {
			if arm.When() != nil {
				retainColumns(arm.When())
			}
		}
	}
	for _, gate := range g.Gates() {
		roots[gate.To()] = true
	}

	live := map[string]bool{}
	visiting := map[string]bool{}
	selected := map[string]wiring{}
	var dependsOn func(string, string, map[string]bool) bool
	dependsOn = func(source, target string, seen map[string]bool) bool {
		if source == target {
			return true
		}
		if seen[source] {
			return false
		}
		seen[source] = true
		w := wirings[source]
		if chosen, ok := selected[source]; ok {
			w = chosen
		}
		for _, edge := range w.in {
			if dependsOn(edge.From(), target, seen) {
				return true
			}
		}
		return false
	}
	var visit func(string) error
	visit = func(alias string) error {
		if live[alias] {
			return nil
		}
		if visiting[alias] {
			return nil // existing explicit cycles retain their normal termination checks
		}
		visiting[alias] = true
		defer delete(visiting, alias)
		node := nodes[alias]
		if node == nil {
			return fmt.Errorf("unknown canonical source %s", alias)
		}
		w, hasWiring := wirings[alias]
		if hasWiring {
			for _, name := range w.required {
				explicit := slices.ContainsFunc(w.in, func(edge Inbound) bool { return edge.As() == name })
				supplied := args.Params[name] != "" || node.Params()[name] != "" || node.Body()[name] != nil || len(node.Fanout()[name]) > 0 || len(g.Fanout()[name]) > 0 || slices.Contains(tupleKeys(node), name)
				if explicit || supplied {
					continue
				}
				choices := []InboundCandidate{}
				for _, candidate := range w.candidates {
					if candidate.As == name {
						choices = append(choices, candidate)
					}
				}
				slices.SortStableFunc(choices, func(a, b InboundCandidate) int {
					if a.Canonical != b.Canonical {
						if a.Canonical {
							return -1
						}
						return 1
					}
					if a.Cost < b.Cost {
						return -1
					}
					if a.Cost > b.Cost {
						return 1
					}
					return 0
				})
				found := false
				for _, candidate := range choices {
					if nodes[candidate.From] == nil || visiting[candidate.From] || dependsOn(candidate.From, alias, map[string]bool{}) {
						continue
					}
					w.in = append(w.in, NewInbound(candidate.From, candidate.Src, candidate.As))
					found = true
					break
				}
				if !found {
					return fmt.Errorf("%s: required inbox input %q has no supplied value, explicit edge, or applicable candidate", alias, name)
				}
			}
			w.candidates = nil
			selected[alias] = w
			for _, edge := range w.in {
				if err := visit(edge.From()); err != nil {
					return err
				}
			}
		}
		live[alias] = true
		return nil
	}
	for _, node := range g.Nodes() {
		if roots[node.Alias()] {
			if err := visit(node.Alias()); err != nil {
				return nil, err
			}
		}
	}
	if len(live) == 0 {
		return nil, fmt.Errorf("canonical candidates have no consumer goal")
	}
	var retainedNodes []Node
	var retainedWirings []Wiring
	for _, node := range g.Nodes() {
		if live[node.Alias()] {
			retainedNodes = append(retainedNodes, node)
		}
	}
	for _, original := range g.Wirings() {
		if chosen, ok := selected[original.To()]; ok {
			if len(chosen.in) > 0 || len(chosen.required) > 0 {
				retainedWirings = append(retainedWirings, chosen)
			}
		}
	}
	terminations := maps.Clone(g.Terminations())
	for alias := range terminations {
		if !live[alias] {
			delete(terminations, alias)
		}
	}
	var projections []Projection
	for _, projection := range g.Projections() {
		if live[projection.Alias()] {
			projections = append(projections, projection)
		}
	}
	return canonicalGraph{Graph: g, nodes: retainedNodes, wirings: retainedWirings, terminations: terminations, projections: projections}, nil
}
