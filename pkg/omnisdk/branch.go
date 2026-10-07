package omnisdk

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stackql-labs/omnisdk/pkg/query"
)

// Arm is one way a branch can go: taken for a row when its condition holds and no earlier arm's does.
type Arm interface {
	Label() string
	// When is the condition; nil takes every row no earlier arm took, and is only allowed last.
	When() query.Predicate
}

// NewArm is an arm taken when when holds.
func NewArm(label string, when query.Predicate) Arm { return arm{label: label, when: when} }

// Otherwise is the last arm: taken by every row no earlier arm took.
func Otherwise(label string) Arm { return arm{label: label} }

type arm struct {
	label string
	when  query.Predicate
}

func (a arm) Label() string         { return a.label }
func (a arm) When() query.Predicate { return a.when }

// Branch chooses, once per row, the first of its arms whose condition holds: if, else if, else. It
// makes no request. What runs on an arm is stated by gates, not by the data, so a value that happens
// to be empty never decides which way a row goes.
type Branch interface {
	Alias() string
	Arms() []Arm
}

// NewBranch declares a branch. Arms are tried in order; an Otherwise arm may only be last.
func NewBranch(alias string, arms ...Arm) (Branch, error) {
	switch {
	case alias == "":
		return nil, fmt.Errorf("omnisdk: a branch needs an alias")
	case strings.ContainsRune(alias, 0):
		return nil, fmt.Errorf("omnisdk: alias %q contains a NUL byte", alias)
	case len(arms) == 0:
		return nil, fmt.Errorf("omnisdk: branch %q has no arms", alias)
	}
	seen := map[string]bool{}
	for i, a := range arms {
		switch {
		case a == nil || a.Label() == "":
			return nil, fmt.Errorf("omnisdk: branch %q has an arm with no label", alias)
		case seen[a.Label()]:
			return nil, fmt.Errorf("omnisdk: branch %q has two arms labelled %q", alias, a.Label())
		case a.When() == nil && i != len(arms)-1:
			return nil, fmt.Errorf("omnisdk: branch %q: arm %q takes every row, so the arms after it could never run; it must be last", alias, a.Label())
		}
		seen[a.Label()] = true
		if a.When() != nil {
			for _, c := range predicateColumns(a.When()) {
				if c.Qualifier() == "" {
					return nil, fmt.Errorf("omnisdk: branch %q, arm %q reads %q unqualified; name its node", alias, a.Label(), c.Name())
				}
			}
		}
	}
	return branch{alias: alias, arms: slices.Clone(arms)}, nil
}

type branch struct {
	alias string
	arms  []Arm
}

func (b branch) Alias() string { return b.alias }
func (b branch) Arms() []Arm   { return b.arms }

// Gate runs node To only on a row where Branch chose Arm. A node with several gates runs when any of
// them is chosen, which is how arms rejoin. A gate is control, not data: nothing flows along it.
type Gate interface {
	Branch() string
	Arm() string
	To() string
}

// NewGate runs to only where branch chose arm.
func NewGate(branch, arm, to string) Gate { return gate{branch: branch, arm: arm, to: to} }

type gate struct{ branch, arm, to string }

func (g gate) Branch() string { return g.branch }
func (g gate) Arm() string    { return g.arm }
func (g gate) To() string     { return g.to }

// WithBranch is g with b added and gates from its arms. Every column an arm reads names a node of g;
// every gate leaves an arm of b and reaches a node of g.
func WithBranch(g Graph, b Branch, gates ...Gate) (Graph, error) {
	gr, ok := g.(graph)
	if !ok {
		return nil, fmt.Errorf("omnisdk: WithBranch needs a graph built by NewGraph")
	}
	if b == nil {
		return nil, fmt.Errorf("omnisdk: branch is nil")
	}
	known := map[string]bool{}
	for _, n := range gr.nodes {
		known[n.Alias()] = true
	}
	switch {
	case known[b.Alias()]:
		return nil, fmt.Errorf("omnisdk: branch %q has the alias of a node", b.Alias())
	case slices.ContainsFunc(gr.branches, func(x Branch) bool { return x.Alias() == b.Alias() }):
		return nil, fmt.Errorf("omnisdk: branch %q is declared twice", b.Alias())
	}
	labels := map[string]bool{}
	for _, a := range b.Arms() {
		labels[a.Label()] = true
		if a.When() == nil {
			continue
		}
		for _, c := range predicateColumns(a.When()) {
			if !known[c.Qualifier()] {
				return nil, fmt.Errorf("omnisdk: branch %q, arm %q reads %s.%s, which the graph does not include", b.Alias(), a.Label(), c.Qualifier(), c.Name())
			}
		}
	}
	for _, x := range gates {
		switch {
		case x == nil:
			return nil, fmt.Errorf("omnisdk: branch %q: a gate is nil", b.Alias())
		case x.Branch() != b.Alias():
			return nil, fmt.Errorf("omnisdk: gate from %q given with branch %q", x.Branch(), b.Alias())
		case !labels[x.Arm()]:
			return nil, fmt.Errorf("omnisdk: branch %q has no arm %q", b.Alias(), x.Arm())
		case !known[x.To()]:
			return nil, fmt.Errorf("omnisdk: gate targets %q, which the graph does not include", x.To())
		}
	}
	gr.branches = append(slices.Clone(gr.branches), b)
	gr.gates = append(slices.Clone(gr.gates), gates...)
	gr.terminate = maps.Clone(gr.terminate)
	return gr, nil
}
