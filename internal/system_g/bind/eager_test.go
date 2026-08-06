package bind

import (
	"context"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// gatedOuter emits `first`, then blocks on `gate` before emitting `rest` — so a test can observe
// whether the join emits first's inner result BEFORE the outer has finished producing.
type gatedOuter struct {
	first facade.Record
	rest  []facade.Record
	gate  chan struct{}
}

func (g *gatedOuter) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1024, 0)
	go func() {
		defer buf.Complete(nil)
		if err := buf.Append(ctx, g.first); err != nil {
			return
		}
		<-g.gate
		for _, r := range g.rest {
			if err := buf.Append(ctx, r); err != nil {
				return
			}
		}
	}()
	return buf.Reader()
}

// TestBindJoinEmitsEagerly proves the Volcano invariant: the join emits an inner result for an early
// outer row WITHOUT first draining the whole outer. The outer withholds its second row behind a gate;
// an eager join yields the first row's inner tuple immediately, a pipeline-breaker would block until
// the gate opens.
func TestBindJoinEmitsEagerly(t *testing.T) {
	gate := make(chan struct{})
	outer := &gatedOuter{
		first: NewDocRecord(map[string]any{"k": "a"}),
		rest:  []facade.Record{NewDocRecord(map[string]any{"k": "b"})},
		gate:  gate,
	}
	inner := InnerFactory(func(bound map[string]any) facade.Operator {
		return sliceOp{recs: []facade.Record{NewDocRecord(map[string]any{"echo": bound["k"]})}}
	})
	join := NewBindJoin(1, outer, []Binding{NewBinding("k", "k")}, inner, nil, 1)

	ctx := context.Background()
	rows := join.Open(ctx)
	defer rows.Close()

	type res struct {
		rec facade.Record
		ok  bool
	}
	got := make(chan res, 1)
	go func() {
		ok := rows.Next(ctx)
		var rec facade.Record
		if ok {
			rec = rows.Record()
		}
		got <- res{rec, ok}
	}()

	// The first result must arrive while the outer is STILL blocked on the gate.
	select {
	case r := <-got:
		if !r.ok {
			t.Fatal("expected an eager record, got EOF/err")
		}
		m, _ := DocMap(r.rec)
		out, _ := m[KeyOutput].(map[string]any)
		if out["echo"] != "a" {
			t.Fatalf("eager record = %v, want echo=a", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bind-join drained the outer before emitting — not eager (pipeline breaker)")
	}

	close(gate) // release the rest of the outer

	if !rows.Next(ctx) {
		t.Fatal("expected the second row after the gate opened")
	}
	m, _ := DocMap(rows.Record())
	out, _ := m[KeyOutput].(map[string]any)
	if out["echo"] != "b" {
		t.Fatalf("second record = %v, want echo=b", out)
	}
}
