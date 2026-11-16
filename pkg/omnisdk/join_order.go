package omnisdk

import (
	"maps"
	"slices"

	"github.com/stackql-labs/omnisdk/pkg/query"
)

// JoinCostTable optionally supplies estimates per read, before local filters
// and fan-out. Zero in either estimate uses the planner's default unit cost.
type JoinCostTable interface {
	Table
	JoinCost() (rows, bytes uint64)
}

type joinCost struct {
	rows, bytes float64
}

func (r *resolver) costedJoinOrder(predicates []query.Predicate) []string {
	// These query forms can depend on the original left-to-right row stream.
	if len(r.order) < 2 || r.mutating != "" || len(r.outer) != 0 || len(r.funcs) != 0 {
		return nil
	}

	// Request bindings are already directed. Only local column equalities are
	// candidates for a reversible hash lookup.
	drives := map[string]bool{}
	for _, inputs := range r.arrivals {
		for _, input := range inputs {
			drives[input.from] = true
		}
	}
	peers := map[string][]string{}
	for _, predicate := range predicates {
		eq, ok := predicate.(query.Compare)
		if !ok || eq.Op() != query.Eq {
			continue
		}
		left, lok := eq.Left().(query.Column)
		right, rok := eq.Right().(query.Column)
		if !lok || !rok {
			continue
		}
		left, lerr := r.qualify(left)
		right, rerr := r.qualify(right)
		if lerr != nil || rerr != nil || left.Qualifier() == right.Qualifier() {
			continue
		}
		a, b := left.Qualifier(), right.Qualifier()
		peers[a] = append(peers[a], b)
		peers[b] = append(peers[b], a)
	}

	costs := map[string]joinCost{}
	for _, alias := range r.order {
		var rows, bytes uint64
		if source, ok := r.tables[alias].(JoinCostTable); ok {
			rows, bytes = source.JoinCost()
		}
		cost := joinCost{float64(max(1, rows)), float64(max(1, bytes))}
		for _, name := range slices.Sorted(maps.Keys(r.fanout[alias])) {
			count := float64(max(1, len(r.fanout[alias][name])))
			cost.rows *= count
			cost.bytes *= count
		}
		costs[alias] = cost
	}

	placed := map[string]bool{}
	order := make([]string, 0, len(r.order))
	for len(order) < len(r.order) {
		best, bestRank, bestCost := "", 5, 0.0
		for _, alias := range r.order {
			if placed[alias] {
				continue
			}
			inputs := r.arrivals[alias]
			ready := true
			for _, input := range inputs {
				ready = ready && placed[input.from]
			}
			if !ready {
				continue
			}
			linked := false
			for _, peer := range peers[alias] {
				linked = linked || placed[peer]
			}

			estimate := costs[alias]
			rank, cost := 4, estimate.rows
			switch {
			case len(inputs) > 0:
				rank = 0
			case linked:
				rank, cost = 1, estimate.bytes
			case drives[alias]:
				rank = 2
			case len(peers[alias]) > 0:
				// Stream the larger input and build the smaller lookup.
				rank, cost = 3, -estimate.bytes
			case len(order) == 0:
				cost = -estimate.bytes
			}
			if rank < bestRank || (rank == bestRank && cost < bestCost) {
				best, bestRank, bestCost = alias, rank, cost
			}
		}
		if best == "" {
			// Leave cyclic graphs to the normal plan validation.
			return nil
		}
		placed[best] = true
		order = append(order, best)
	}
	return order
}
