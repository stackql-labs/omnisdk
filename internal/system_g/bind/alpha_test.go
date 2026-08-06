package bind

import (
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// A bind join with a delay edge must wait (at least) the delay before invoking the inner.
func TestBindJoinHonoursDelay(t *testing.T) {
	const delay = 60 * time.Millisecond
	outer := sliceOp{recs: []facade.Record{NewDocRecord(map[string]any{"k": "v"})}}
	inner := func(map[string]any) facade.Operator {
		return sliceOp{recs: []facade.Record{NewDocRecord(map[string]any{"ok": true})}}
	}
	join := NewBindJoin(1, outer, []Binding{NewBinding("k", "k")}, inner, NewDelayEdge(delay), 1)

	start := time.Now()
	rows := collectDocs(t, join)
	elapsed := time.Since(start)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if elapsed < delay {
		t.Errorf("elapsed %s < delay %s — delay not honoured", elapsed, delay)
	}
}
