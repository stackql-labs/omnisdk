package scc

import (
	"context"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/termination"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

const nodeKey = "id"

func node(id int) facade.Record {
	return record.NewRecord(map[string]facade.Value{
		nodeKey: value.NewBytesValue([]byte(strconv.Itoa(id))),
	})
}

func nodeID(r facade.Record) int {
	b, _ := io.ReadAll(r.Get(nodeKey).Reader())
	n, _ := strconv.Atoi(string(b))
	return n
}

// sliceOp is a seed operator that yields a fixed slice of records once.
type sliceOp struct{ recs []facade.Record }

func (o sliceOp) Open(context.Context) facade.Records { return &sliceCursor{recs: o.recs, i: -1} }

type sliceCursor struct {
	recs []facade.Record
	i    int
}

func (c *sliceCursor) Next(context.Context) bool { c.i++; return c.i < len(c.recs) }
func (c *sliceCursor) Record() facade.Record     { return c.recs[c.i] }
func (c *sliceCursor) Err() error                { return nil }
func (c *sliceCursor) Close() error              { return nil }

// reachStep derives the direct successors of every node in the frontier.
type reachStep struct{ adj map[int][]int }

func (s reachStep) Round(_ context.Context, frontier []facade.Record) ([]facade.Record, error) {
	var out []facade.Record
	for _, r := range frontier {
		for _, succ := range s.adj[nodeID(r)] {
			out = append(out, node(succ))
		}
	}
	return out, nil
}

// Reachability over 0→{1,2}, 1→3, 2→3, 3→∅. The SCC must emit the full closure
// {0,1,2,3} exactly once each (dedup collapses the two paths into 3), converge by
// fixpoint (no policy), and take three rounds.
func TestSCCTransitiveClosureFixpoint(t *testing.T) {
	adj := map[int][]int{
		0: {1, 2},
		1: {3},
		2: {3},
		3: nil,
	}
	op := NewSCC(
		0,
		sliceOp{recs: []facade.Record{node(0)}},
		reachStep{adj: adj},
		func(r facade.Record) string { return strconv.Itoa(nodeID(r)) },
		nil, // rely on fixpoint only
		1,
	)

	ctx := context.Background()
	rs := op.Open(ctx)
	defer rs.Close()

	got := map[int]int{}
	for rs.Next(ctx) {
		got[nodeID(rs.Record())]++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}

	want := []int{0, 1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("closure = %v, want nodes %v", got, want)
	}
	for _, n := range want {
		if got[n] != 1 {
			t.Errorf("node %d emitted %d times, want exactly 1", n, got[n])
		}
	}

	s := op.(*scc)
	if r := s.TerminationReason(); r != Fixpoint {
		t.Errorf("TerminationReason = %d, want Fixpoint (%d)", r, Fixpoint)
	}
	if it := s.Iterations(); it != 3 {
		t.Errorf("Iterations = %d, want 3", it)
	}
}

// succStep derives exactly one brand-new node per input (n → n+1). Every id is unique,
// so dedup never collapses anything and the frontier never empties: this cycle has no
// fixpoint and would iterate forever without a backstop.
type succStep struct{}

func (succStep) Round(_ context.Context, frontier []facade.Record) ([]facade.Record, error) {
	out := make([]facade.Record, 0, len(frontier))
	for _, r := range frontier {
		out = append(out, node(nodeID(r)+1))
	}
	return out, nil
}

// A non-converging cycle must still terminate — bounded by the well-founded policy, not
// by fixpoint. With MaxIterations(5): the seed emits node 0, five rounds emit nodes 1..5,
// and the sixth round is refused by the policy before it runs. The context timeout is a
// safety net: if the backstop were broken the stream would be infinite and we'd fail on
// ctx rather than hang the suite.
func TestSCCNonConvergingTerminatesOnPolicy(t *testing.T) {
	const rounds = 5

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	op := NewSCC(
		0,
		sliceOp{recs: []facade.Record{node(0)}},
		succStep{},
		func(r facade.Record) string { return strconv.Itoa(nodeID(r)) },
		termination.NewMaxIterations(rounds),
		1,
	)

	rs := op.Open(ctx)
	defer rs.Close()

	got := map[int]int{}
	for rs.Next(ctx) {
		got[nodeID(rs.Record())]++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatalf("did not terminate on its own: %v", ctx.Err())
	}

	// Exactly nodes 0..rounds, once each (seed + one per bounded round).
	if len(got) != rounds+1 {
		t.Fatalf("emitted %v, want nodes 0..%d once each", got, rounds)
	}
	for n := 0; n <= rounds; n++ {
		if got[n] != 1 {
			t.Errorf("node %d emitted %d times, want exactly 1", n, got[n])
		}
	}

	s := op.(*scc)
	if r := s.TerminationReason(); r != Terminated {
		t.Errorf("TerminationReason = %d, want Terminated (%d)", r, Terminated)
	}
	if it := s.Iterations(); it != rounds {
		t.Errorf("Iterations = %d, want %d", it, rounds)
	}
}
