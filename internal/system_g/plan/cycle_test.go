package plan

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/termination"
)

// rowsOf is an exchange answering each input with rows computed from it, counting its runs.
func rowsOf(name string, in []string, calls *atomic.Int64, fn func(bound map[string]any) []map[string]any) ExchangeSpec {
	return NewExchangeSpec(name, in, nil, func(bound map[string]any) facade.Operator {
		if calls != nil {
			calls.Add(1)
		}
		return staticRecords(func() []facade.Record {
			var out []facade.Record
			for _, r := range fn(bound) {
				out = append(out, bind.NewDocRecord(r))
			}
			return out
		}())
	}, bind.NewInnerFlatten())
}

func collect(t *testing.T, p Plan) ([]map[string]any, error) {
	t.Helper()
	rs := ComposeRows(1, p).Open(context.Background())
	defer rs.Close()
	var out []map[string]any
	for rs.Next(context.Background()) {
		m, _ := bind.DocMap(rs.Record())
		out = append(out, m)
	}
	return out, rs.Err()
}

// tree answers the children of a folder, so a self-wired crawl reaches every descendant.
var tree = map[string][]string{"root": {"a", "b"}, "a": {"c"}, "b": nil, "c": nil}

func crawl(calls *atomic.Int64) ExchangeSpec {
	return rowsOf("folders", []string{"parent"}, calls, func(b map[string]any) []map[string]any {
		var out []map[string]any
		for _, c := range tree[fmt.Sprint(b["parent"])] {
			out = append(out, map[string]any{"id": c})
		}
		return out
	})
}

// The first value comes from outside the cycle, each later one from the member's own output.
var crawlEdges = []BetaEdge{NewBetaEdge("", "folders", "start", "parent"), NewBetaEdge("folders", "folders", "id", "parent")}

func ids(rows []map[string]any) string {
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprint(r["id"]))
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

// A self-wired exchange crawls to a fixpoint: every descendant, then it stops because a round
// derives nothing new.
func TestCycleReachesItsFixpoint(t *testing.T) {
	p := WithTerminations(NewPlan([]ExchangeSpec{crawl(nil)}, crawlEdges, nil, map[string]any{"start": "root"}, nil, nil),
		map[string]facade.TerminationSpec{"folders": termination.Rounds(10)})
	rows, err := collect(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(rows); got != "a,b,c" {
		t.Errorf("ids = %s, want a,b,c", got)
	}
}

// A cycle with no termination policy, or one that is not well-founded, is refused before any
// exchange runs.
func TestCycleNeedsAWellFoundedPolicy(t *testing.T) {
	for name, specs := range map[string]map[string]facade.TerminationSpec{
		"none":        nil,
		"zero rounds": {"folders": termination.Rounds(0)},
		"all with an unbounded condition": {"folders": termination.AllOf(termination.Rounds(5),
			termination.Records(0))},
	} {
		var calls atomic.Int64
		p := WithTerminations(NewPlan([]ExchangeSpec{crawl(&calls)}, crawlEdges, nil, map[string]any{"start": "root"}, nil, nil), specs)
		_, err := collect(t, p)
		if err == nil || !strings.Contains(err.Error(), "cycle through folders") {
			t.Errorf("%s: err = %v, want the cycle refused", name, err)
		}
		if calls.Load() != 0 {
			t.Errorf("%s: %d runs before the refusal", name, calls.Load())
		}
	}
}

// A cycle that derives something new every round stops at its bound.
func TestCycleStopsAtItsBound(t *testing.T) {
	counter := rowsOf("next", []string{"n"}, nil, func(b map[string]any) []map[string]any {
		var n int
		fmt.Sscan(fmt.Sprint(b["n"]), &n)
		return []map[string]any{{"v": n + 1}}
	})
	p := WithTerminations(NewPlan([]ExchangeSpec{counter},
		[]BetaEdge{NewBetaEdge("", "next", "start", "n"), NewBetaEdge("next", "next", "v", "n")},
		nil, map[string]any{"start": 0}, nil, nil),
		map[string]facade.TerminationSpec{"next": termination.Rounds(3)})
	rows, err := collect(t, p)
	if err != nil {
		t.Fatal(err)
	}
	// The entry's run, then three rounds.
	if len(rows) != 4 {
		t.Errorf("rows = %v, want 4", rows)
	}
}

// Two exchanges feeding each other form one cycle, entered where a value arrives from outside it.
func TestMutualCycle(t *testing.T) {
	a := rowsOf("A", []string{"x"}, nil, func(b map[string]any) []map[string]any {
		var n int
		fmt.Sscan(fmt.Sprint(b["x"]), &n)
		if n >= 4 {
			return nil
		}
		return []map[string]any{{"a": n + 1}}
	})
	b := rowsOf("B", []string{"y"}, nil, func(b map[string]any) []map[string]any {
		var n int
		fmt.Sscan(fmt.Sprint(b["y"]), &n)
		return []map[string]any{{"b": n + 1}}
	})
	p := WithTerminations(NewPlan([]ExchangeSpec{a, b},
		[]BetaEdge{NewBetaEdge("", "A", "start", "x"), NewBetaEdge("B", "A", "b", "x"), NewBetaEdge("A", "B", "a", "y")},
		nil, map[string]any{"start": 0}, nil, nil),
		map[string]facade.TerminationSpec{"B": termination.Records(100)})
	rows, err := collect(t, p)
	if err != nil {
		t.Fatal(err)
	}
	// A: 0→1, B: 1→2, A: 2→3, B: 3→4, A: 4 stops.
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("a=%v b=%v", r["a"], r["b"]))
	}
	if len(rows) != 4 {
		t.Errorf("rows = %v, want A,B,A,B", got)
	}
}

// A cycle every member of which waits on another member has nothing to start from, and is refused.
func TestCycleWithoutAnEntryIsRefused(t *testing.T) {
	a := rowsOf("A", []string{"x"}, nil, func(map[string]any) []map[string]any { return nil })
	b := rowsOf("B", []string{"y"}, nil, func(map[string]any) []map[string]any { return nil })
	p := WithTerminations(NewPlan([]ExchangeSpec{a, b},
		[]BetaEdge{NewBetaEdge("B", "A", "b", "x"), NewBetaEdge("A", "B", "a", "y")}, nil, nil, nil, nil),
		map[string]facade.TerminationSpec{"A": termination.Rounds(3)})
	if _, err := collect(t, p); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Errorf("err = %v, want no entry", err)
	}
}

// A stage after the cycle binds from every row the cycle reached.
func TestStageAfterACycle(t *testing.T) {
	size := rowsOf("size", []string{"folder"}, nil, func(b map[string]any) []map[string]any {
		return []map[string]any{{"bytes": len(fmt.Sprint(b["folder"]))}}
	})
	p := WithTerminations(NewPlan([]ExchangeSpec{crawl(nil), size},
		append(slices.Clone(crawlEdges), NewBetaEdge("folders", "size", "id", "folder")),
		nil, map[string]any{"start": "root"}, nil, nil),
		map[string]facade.TerminationSpec{"folders": termination.Rounds(10)})
	rows, err := collect(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(rows); got != "a,b,c" {
		t.Errorf("ids = %s", got)
	}
	for _, r := range rows {
		if r["bytes"] != 1 {
			t.Errorf("row %v not sized", r)
		}
	}
}
