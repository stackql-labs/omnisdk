// Package descent turns a recursive (self-β) exchange into a bounded traversal. A "list-children"
// exchange whose output rows feed back as its own input (list children → children → list children …)
// is a data-flow cycle; rather than recurse with unbounded pull, it runs as an SCC semi-naive
// fixpoint (see the scc package). So GCP folder trees, AWS OU nesting, and pagination are ALL the
// same shape and reuse this — no per-provider recursion. The operator emits the transitive closure
// (seed + every derived node), streaming, deduped by a key field, with a well-founded backstop.
package descent

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/scc"
	"github.com/stackql-labs/omnisdk/internal/system_g/schedule"
)

// NewRecursive wires a recursive descent over a self-β edge:
//   - seed supplies the initial frontier (e.g. the org node, carrying any auth it needs).
//   - child(bound) lists the children of ONE node; bound holds the fields named by the bindings'
//     Tgt (e.g. parent, token).
//   - bindings copy a frontier node's fields into the child request (e.g. node→parent, token→token).
//     These same fields are carried forward onto every child row, so constants like an auth token
//     propagate through the whole descent without the API having to echo them back.
//   - keyField dedups nodes for fixpoint detection (and breaks accidental cycles).
//   - policy is the well-founded backstop for unbounded/adversarial trees (nil = fixpoint only).
//
// The result is an ordinary pull Operator emitting the closure — compose bind-joins downstream to
// run the per-leaf resource query (the "visitor") over it.
func NewRecursive(id int64, seed facade.Operator, child bind.InnerFactory, bindings []bind.Binding, keyField string, policy facade.TerminationPolicy, readers int) facade.Operator {
	keyer := func(r facade.Record) string {
		m, ok := bind.DocMap(r)
		if !ok {
			return ""
		}
		return fmt.Sprint(m[keyField])
	}
	return scc.NewSCC(id, seed, edgeStep{child: child, bindings: bindings}, keyer, policy, readers)
}

// edgeStep runs one round of the cycle: it fans the child exchange out over the frontier (each node
// an independent scheduler unit, concurrency gated by the ctx limiter — the same fan-out as a
// bind-join) and returns every child row as the round's derived records, with the parent's bound
// fields merged in (child wins) so auth/scope constants ride along to the next round.
type edgeStep struct {
	child    bind.InnerFactory
	bindings []bind.Binding
}

func (s edgeStep) Round(ctx context.Context, frontier []facade.Record) ([]facade.Record, error) {
	var mu sync.Mutex
	var out []facade.Record
	collect := func(r facade.Record) {
		mu.Lock()
		out = append(out, r)
		mu.Unlock()
	}
	nodes := make([]schedule.Node, 0, len(frontier))
	for i, rec := range frontier {
		row, ok := bind.DocMap(rec)
		if !ok {
			continue
		}
		bound := make(map[string]any, len(s.bindings))
		for _, b := range s.bindings {
			bound[b.Tgt()] = row[b.Src()]
		}
		nodes = append(nodes, &childNode{name: strconv.Itoa(i), bound: bound, child: s.child, collect: collect})
	}
	if err := schedule.Run(ctx, nodes, schedule.LimiterFrom(ctx)); err != nil {
		return nil, err
	}
	return out, nil
}

// childNode drives the child exchange for one frontier node, merging the node's bound fields onto
// each child row so binding constants (token, scope) carry forward through the recursion.
type childNode struct {
	name    string
	bound   map[string]any
	child   bind.InnerFactory
	collect func(facade.Record)
}

func (n *childNode) Name() string        { return n.name }
func (n *childNode) DependsOn() []string { return nil }

func (n *childNode) Run(ctx context.Context) error {
	in := n.child(n.bound).Open(ctx)
	defer in.Close()
	for in.Next(ctx) {
		row, ok := bind.DocMap(in.Record())
		if !ok {
			continue
		}
		merged := make(map[string]any, len(row)+len(n.bound))
		for k, v := range n.bound {
			merged[k] = v
		}
		for k, v := range row { // child fields win over carried-forward parent fields
			merged[k] = v
		}
		n.collect(bind.NewDocRecord(merged))
	}
	return in.Err()
}
