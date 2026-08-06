// Package schedule runs a dependency DAG optimistically: each node starts as soon as the nodes it
// depends on have completed, so independent nodes run concurrently. Concurrency is gated by an
// admission facade.Limiter (the policy) — nil means unlimited. Deliberately small: optimistic,
// dependency-gated, policy-gated execution, nothing more. It is a standalone, swappable executor;
// the graph layer supplies Nodes and the scope-keyed Limiter.
package schedule

import (
	"context"
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Node is a unit of work in the DAG: its name, the names it depends on (which must complete
// before it starts), and the work to run.
type Node interface {
	Name() string
	DependsOn() []string
	Run(ctx context.Context) error
}

// Run executes nodes respecting dependencies, gated by lim (nil = unlimited). It returns the
// first node error; on error it stops launching new nodes, cancels in-flight ones via ctx, and
// waits for them to finish. A duplicate, missing, or cyclic dependency is rejected before any node
// runs — no silent drops.
func Run(ctx context.Context, nodes []Node, lim facade.Limiter) error {
	byName := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		if _, dup := byName[n.Name()]; dup {
			return fmt.Errorf("schedule: duplicate node %q", n.Name())
		}
		byName[n.Name()] = n
	}
	indeg := make(map[string]int, len(nodes))
	dependents := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		for _, dep := range n.DependsOn() {
			if _, ok := byName[dep]; !ok {
				return fmt.Errorf("schedule: node %q depends on unknown %q", n.Name(), dep)
			}
			indeg[n.Name()]++
			dependents[dep] = append(dependents[dep], n.Name())
		}
	}
	if cyclic(nodes, indeg, dependents) {
		return fmt.Errorf("schedule: dependency cycle among nodes")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		name string
		err  error
	}
	// The coordinator loop below is the sole owner of indeg/running/firstErr — worker goroutines
	// only run their node and report on doneCh, so there is no shared mutable state to guard.
	doneCh := make(chan result)
	running := 0
	launch := func(n Node) {
		running++
		go func() { doneCh <- result{n.Name(), runOne(ctx, n, lim)} }()
	}
	for _, n := range nodes {
		if indeg[n.Name()] == 0 {
			launch(n)
		}
	}
	var firstErr error
	for running > 0 {
		r := <-doneCh
		running--
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
				cancel() // stop the rest; in-flight nodes observe ctx.Done
			}
			continue
		}
		if firstErr != nil {
			continue // draining after an error: launch nothing new
		}
		for _, m := range dependents[r.name] {
			indeg[m]--
			if indeg[m] == 0 {
				launch(byName[m])
			}
		}
	}
	return firstErr
}

// runOne acquires an admission slot (if lim != nil), runs the node, and releases the slot.
func runOne(ctx context.Context, n Node, lim facade.Limiter) error {
	if lim != nil {
		tok, err := lim.Acquire(ctx)
		if err != nil {
			return err
		}
		defer tok.Release()
	}
	return n.Run(ctx)
}

type limKey struct{}

// WithLimiter carries the fan-out concurrency limiter on ctx (nil = unlimited).
func WithLimiter(ctx context.Context, lim facade.Limiter) context.Context {
	if lim == nil {
		return ctx
	}
	return context.WithValue(ctx, limKey{}, lim)
}

// LimiterFrom returns the limiter carried on ctx, or nil (unlimited).
func LimiterFrom(ctx context.Context) facade.Limiter {
	if l, ok := ctx.Value(limKey{}).(facade.Limiter); ok {
		return l
	}
	return nil
}

// cyclic reports whether the dependency graph has a cycle (Kahn's fails to reach every node).
func cyclic(nodes []Node, indeg map[string]int, dependents map[string][]string) bool {
	deg := make(map[string]int, len(indeg))
	for k, v := range indeg {
		deg[k] = v
	}
	var queue []string
	for _, n := range nodes {
		if deg[n.Name()] == 0 {
			queue = append(queue, n.Name())
		}
	}
	seen := 0
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		seen++
		for _, m := range dependents[x] {
			deg[m]--
			if deg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}
	return seen != len(nodes)
}
