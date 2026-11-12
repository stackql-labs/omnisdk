package plan

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestCheckpointsWaitForEveryRowInSection(t *testing.T) {
	var a, b, done atomic.Int64
	root := rowsOf("root", []string{"seed"}, nil, func(map[string]any) []map[string]any { return []map[string]any{{"x": 1}, {"x": 2}, {"x": 3}} })
	member := func(name string, calls *atomic.Int64) ExchangeSpec {
		spec := rowsOf(name, []string{"x"}, calls, func(row map[string]any) []map[string]any { return []map[string]any{{name: row["x"]}} })
		return WithCheckpoint(spec, func(context.Context) error {
			if calls.Load() != 3 {
				return fmt.Errorf("%s checkpoint after %d/3 rows", name, calls.Load())
			}
			done.Add(1)
			return nil
		})
	}
	p := NewPlan([]ExchangeSpec{root, member("a", &a), member("b", &b)}, []BetaEdge{
		NewBetaEdge("", "root", "seed", "seed"), NewBetaEdge("root", "a", "x", "x"), NewBetaEdge("root", "b", "x", "x"),
	}, nil, map[string]any{"seed": true}, nil, nil)
	rows, err := collect(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || done.Load() != 2 {
		t.Fatalf("rows=%d checkpoints=%d", len(rows), done.Load())
	}
}

func TestFailedCheckpointPropagates(t *testing.T) {
	spec := WithCheckpoint(rowsOf("root", []string{"seed"}, nil, func(map[string]any) []map[string]any { return []map[string]any{{"x": 1}} }), func(context.Context) error { return fmt.Errorf("checkpoint persistence failed") })
	p := NewPlan([]ExchangeSpec{spec}, []BetaEdge{NewBetaEdge("", "root", "seed", "seed")}, nil, map[string]any{"seed": true}, nil, nil)
	if _, err := collect(t, p); err == nil {
		t.Fatal("checkpoint failure lost")
	}
}
