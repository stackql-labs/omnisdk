package schedule

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/admit"
)

type node struct {
	name string
	deps []string
	run  func(ctx context.Context) error
}

func (n node) Name() string                { return n.name }
func (n node) DependsOn() []string         { return n.deps }
func (n node) Run(c context.Context) error { return n.run(c) }

func rec(order *[]string, mu *sync.Mutex, name string) func(context.Context) error {
	return func(context.Context) error {
		mu.Lock()
		*order = append(*order, name)
		mu.Unlock()
		return nil
	}
}

// Dependencies are honoured: a node never runs before the nodes it depends on.
func TestRunRespectsDependencies(t *testing.T) {
	var mu sync.Mutex
	var order []string
	nodes := []Node{
		node{name: "A", run: rec(&order, &mu, "A")},
		node{name: "B", deps: []string{"A"}, run: rec(&order, &mu, "B")},
		node{name: "C", deps: []string{"B"}, run: rec(&order, &mu, "C")},
		node{name: "D", deps: []string{"A"}, run: rec(&order, &mu, "D")}, // diamond-ish: D also needs A
	}
	if err := Run(context.Background(), nodes, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	pos := map[string]int{}
	for i, n := range order {
		pos[n] = i
	}
	if !(pos["A"] < pos["B"] && pos["B"] < pos["C"] && pos["A"] < pos["D"]) {
		t.Errorf("order %v violates dependencies", order)
	}
}

// Independent nodes run concurrently (optimistic).
func TestRunParallelIndependent(t *testing.T) {
	var cur, max int32
	work := func(context.Context) error {
		c := atomic.AddInt32(&cur, 1)
		for {
			m := atomic.LoadInt32(&max)
			if c <= m || atomic.CompareAndSwapInt32(&max, m, c) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		return nil
	}
	var nodes []Node
	for i := 0; i < 10; i++ {
		nodes = append(nodes, node{name: string(rune('a' + i)), run: work})
	}
	if err := Run(context.Background(), nodes, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if max < 2 {
		t.Errorf("max concurrency %d — independent nodes did not run in parallel", max)
	}
}

// Admission gates concurrency: a semaphore of 2 caps parallel nodes at 2.
func TestRunAdmissionGates(t *testing.T) {
	var cur, max int32
	work := func(context.Context) error {
		c := atomic.AddInt32(&cur, 1)
		for {
			m := atomic.LoadInt32(&max)
			if c <= m || atomic.CompareAndSwapInt32(&max, m, c) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		return nil
	}
	var nodes []Node
	for i := 0; i < 12; i++ {
		nodes = append(nodes, node{name: string(rune('a' + i)), run: work})
	}
	if err := Run(context.Background(), nodes, admit.NewSemaphore(2)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if max > 2 {
		t.Errorf("observed %d concurrent nodes, want <= 2 (admission breached)", max)
	}
}

// A node error propagates, and dependents of the failed node do not run.
func TestRunPropagatesError(t *testing.T) {
	boom := errors.New("boom")
	var ranC int32
	nodes := []Node{
		node{name: "A", run: func(context.Context) error { return boom }},
		node{name: "C", deps: []string{"A"}, run: func(context.Context) error { atomic.AddInt32(&ranC, 1); return nil }},
	}
	if err := Run(context.Background(), nodes, nil); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if atomic.LoadInt32(&ranC) != 0 {
		t.Error("dependent of a failed node must not run")
	}
}

func TestRunRejectsCycle(t *testing.T) {
	nodes := []Node{
		node{name: "A", deps: []string{"B"}, run: func(context.Context) error { return nil }},
		node{name: "B", deps: []string{"A"}, run: func(context.Context) error { return nil }},
	}
	if err := Run(context.Background(), nodes, nil); err == nil {
		t.Error("a dependency cycle must be rejected")
	}
}

func TestRunRejectsUnknownDep(t *testing.T) {
	nodes := []Node{node{name: "A", deps: []string{"ghost"}, run: func(context.Context) error { return nil }}}
	if err := Run(context.Background(), nodes, nil); err == nil {
		t.Error("an unknown dependency must be rejected")
	}
}
