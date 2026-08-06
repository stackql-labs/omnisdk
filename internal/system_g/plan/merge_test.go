package plan_test

import (
	"context"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
)

// MergeRows presents N disjoint row sources as ONE cursor — every row from every source, once.
func TestMergeRowsFansDisjointGraphsIntoOneOutput(t *testing.T) {
	src := func(names ...string) facade.Operator {
		recs := make([]facade.Record, len(names))
		for i, n := range names {
			recs[i] = bind.NewDocRecord(map[string]any{"name": n})
		}
		return exchange.NewLiteralSource(recs, 1)
	}
	op := plan.MergeRows(src("a", "b"), src("c"), src("d", "e"))

	got := map[string]bool{}
	rs := op.Open(context.Background())
	defer rs.Close()
	for rs.Next(context.Background()) {
		m, _ := bind.DocMap(rs.Record())
		got[m["name"].(string)] = true
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if !got[n] {
			t.Errorf("missing %q from merged output: %v", n, got)
		}
	}
	if len(got) != 5 {
		t.Fatalf("got %d distinct rows, want 5: %v", len(got), got)
	}
}
