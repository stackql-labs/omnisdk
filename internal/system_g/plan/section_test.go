package plan

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Two exchanges with nothing between them run at once: each waits until both have started, which
// one after the other never happens.
func TestIndependentExchangesRunTogether(t *testing.T) {
	var started sync.WaitGroup
	started.Add(2)
	both := make(chan struct{})
	go func() { started.Wait(); close(both) }()
	meet := func(name string) ExchangeSpec {
		return rowsOf(name, []string{"seed"}, nil, func(map[string]any) []map[string]any {
			started.Done()
			select {
			case <-both:
				return []map[string]any{{name: "met"}}
			case <-time.After(2 * time.Second):
				return []map[string]any{{name: "alone"}}
			}
		})
	}
	p := NewPlan([]ExchangeSpec{meet("a"), meet("b")},
		[]BetaEdge{NewBetaEdge("", "a", "seed", "seed"), NewBetaEdge("", "b", "seed", "seed")},
		nil, map[string]any{"seed": "x"}, nil, nil)
	rows, err := collect(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["a"] != "met" || rows[0]["b"] != "met" {
		t.Errorf("rows = %v, want one row in which a and b each met the other", rows)
	}
}

// Independent stages emit what they would one after the other: every combination of their rows,
// merged over the row they ran on, and a stage depending on both sees each combination.
func TestSectionEmitsEveryCombination(t *testing.T) {
	var after atomic.Int64
	p := NewPlan([]ExchangeSpec{
		rowsOf("a", []string{"seed"}, nil, func(map[string]any) []map[string]any {
			return []map[string]any{{"x": 1}, {"x": 2}}
		}),
		rowsOf("b", []string{"seed"}, nil, func(map[string]any) []map[string]any {
			return []map[string]any{{"y": "p"}, {"y": "q"}, {"y": "r"}}
		}),
		rowsOf("c", []string{"x", "y"}, &after, func(b map[string]any) []map[string]any {
			return []map[string]any{{"xy": fmt.Sprint(b["x"], b["y"])}}
		}),
	}, []BetaEdge{
		NewBetaEdge("", "a", "seed", "seed"), NewBetaEdge("", "b", "seed", "seed"),
		NewBetaEdge("a", "c", "x", "x"), NewBetaEdge("b", "c", "y", "y"),
	}, nil, map[string]any{"seed": "s"}, nil, nil)
	rows, err := collect(t, p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[fmt.Sprint(r["xy"])] = r["seed"] == "s"
	}
	if len(rows) != 6 || len(got) != 6 || after.Load() != 6 {
		t.Errorf("rows = %v, c ran %d times; want all six combinations, each over the seed", rows, after.Load())
	}
}

// A failing member stops the section: its sibling, already running, finishes rather than being
// cut off, and nothing downstream runs.
func TestSectionFailureLetsTheRunningFinish(t *testing.T) {
	var finished, downstream atomic.Int64
	slow := rowsOf("slow", []string{"seed"}, nil, func(map[string]any) []map[string]any {
		time.Sleep(50 * time.Millisecond)
		finished.Add(1)
		return []map[string]any{{"slow": true}}
	})
	broken := NewExchangeSpec("broken", []string{"seed"}, nil, func(map[string]any) facade.Operator {
		return errorOp{err: errors.New("boom")}
	}, bind.NewInnerFlatten())
	p := NewPlan([]ExchangeSpec{slow, broken,
		rowsOf("next", []string{"slow"}, &downstream, func(map[string]any) []map[string]any { return nil }),
	}, []BetaEdge{
		NewBetaEdge("", "slow", "seed", "seed"), NewBetaEdge("", "broken", "seed", "seed"),
		NewBetaEdge("slow", "next", "slow", "slow"),
	}, nil, map[string]any{"seed": "s"}, nil, nil)
	if _, err := collect(t, p); err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want boom", err)
	}
	if finished.Load() != 1 || downstream.Load() != 0 {
		t.Errorf("slow finished %d times, downstream ran %d; want the sibling finished and nothing after", finished.Load(), downstream.Load())
	}
}
