package descent

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/termination"
)

func node(fields map[string]any) facade.Record { return bind.NewDocRecord(fields) }

// childrenFrom returns a child factory that lists a static tree keyed by the bound "parent".
func childrenFrom(tree map[string][]map[string]any) bind.InnerFactory {
	return func(bound map[string]any) facade.Operator {
		kids := tree[fmt.Sprint(bound["parent"])]
		recs := make([]facade.Record, len(kids))
		for i, k := range kids {
			recs[i] = node(k)
		}
		return exchange.NewLiteralSource(recs, 1)
	}
}

func drainNodes(t *testing.T, op facade.Operator) []string {
	t.Helper()
	ctx := context.Background()
	in := op.Open(ctx)
	defer in.Close()
	var got []string
	for in.Next(ctx) {
		m, ok := bind.DocMap(in.Record())
		if !ok {
			t.Fatalf("non-doc record")
		}
		got = append(got, fmt.Sprint(m["node"]))
	}
	if err := in.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	sort.Strings(got)
	return got
}

func TestRecursiveEmitsClosure(t *testing.T) {
	tree := map[string][]map[string]any{
		"root": {{"node": "a"}, {"node": "b"}},
		"a":    {{"node": "c"}},
		"c":    {{"node": "d"}},
		// b, d are leaves
	}
	op := NewRecursive(1,
		exchange.NewLiteralSource([]facade.Record{node(map[string]any{"node": "root"})}, 1),
		childrenFrom(tree),
		[]bind.Binding{bind.NewBinding("node", "parent")},
		"node", nil, 1)

	got := drainNodes(t, op)
	want := []string{"a", "b", "c", "d", "root"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("closure = %v, want %v", got, want)
	}
}

func TestRecursiveDedupsCycle(t *testing.T) {
	// root → a → root (cycle): the key dedup must break it and reach a fixpoint.
	tree := map[string][]map[string]any{
		"root": {{"node": "a"}},
		"a":    {{"node": "root"}},
	}
	op := NewRecursive(1,
		exchange.NewLiteralSource([]facade.Record{node(map[string]any{"node": "root"})}, 1),
		childrenFrom(tree),
		[]bind.Binding{bind.NewBinding("node", "parent")},
		"node", nil, 1)

	got := drainNodes(t, op)
	want := []string{"a", "root"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("closure = %v, want %v", got, want)
	}
}

func TestRecursivePolicyBackstop(t *testing.T) {
	// An unbounded tree: every node has exactly one fresh child, so there is no fixpoint. The
	// termination policy must bound it.
	child := bind.InnerFactory(func(bound map[string]any) facade.Operator {
		return exchange.NewLiteralSource([]facade.Record{
			node(map[string]any{"node": fmt.Sprint(bound["parent"]) + "-0"}),
		}, 1)
	})
	op := NewRecursive(1,
		exchange.NewLiteralSource([]facade.Record{node(map[string]any{"node": "n"})}, 1),
		child,
		[]bind.Binding{bind.NewBinding("node", "parent")},
		"node", termination.NewMaxIterations(3), 1)

	got := drainNodes(t, op)
	// seed + 3 rounds of one node each = 4; the policy stops it there instead of forever.
	if len(got) != 4 {
		t.Fatalf("bounded closure size = %d (%v), want 4", len(got), got)
	}
}

func TestRecursiveCarriesConstantsForward(t *testing.T) {
	// token is a binding (token→token) but never echoed by the child rows; it must still propagate
	// to every node so a downstream auth'd query can read it (the auth-through-recursion case).
	tree := map[string][]map[string]any{
		"org": {{"node": "f1"}},
		"f1":  {{"node": "f2"}},
	}
	op := NewRecursive(1,
		exchange.NewLiteralSource([]facade.Record{node(map[string]any{"node": "org", "token": "T"})}, 1),
		childrenFrom(tree),
		[]bind.Binding{bind.NewBinding("node", "parent"), bind.NewBinding("token", "token")},
		"node", nil, 1)

	ctx := context.Background()
	in := op.Open(ctx)
	defer in.Close()
	seen := 0
	for in.Next(ctx) {
		m, _ := bind.DocMap(in.Record())
		if m["token"] != "T" {
			t.Fatalf("node %v lost the carried token: %v", m["node"], m["token"])
		}
		seen++
	}
	if err := in.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if seen != 3 { // org, f1, f2
		t.Fatalf("saw %d nodes, want 3", seen)
	}
}
