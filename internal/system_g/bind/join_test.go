package bind

import (
	"context"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// sliceOp is a minimal outer/inner source over a fixed record slice (no buffer needed).
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

func collectDocs(t *testing.T, op facade.Operator) []map[string]any {
	t.Helper()
	ctx := context.Background()
	rs := op.Open(ctx)
	defer rs.Close()
	var out []map[string]any
	for rs.Next(ctx) {
		m, ok := DocMap(rs.Record())
		if !ok {
			t.Fatalf("record is not an agnostic map")
		}
		out = append(out, m)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	return out
}

// The bind join must (1) copy the bound slots per outer row (β load-bearing), (2) pair inner
// over outer as (input, output) tuples WITHOUT merging, and (3) be left-outer: an outer row
// whose inner is empty still emits (input, null). alpha gets an encryption row; beta empty.
func TestBindJoinTupleLeftOuter(t *testing.T) {
	outer := sliceOp{recs: []facade.Record{
		NewDocRecord(map[string]any{"name": "alpha", "region": "us-east-1"}),
		NewDocRecord(map[string]any{"name": "beta", "region": "us-west-2"}),
	}}

	var mu sync.Mutex
	sawBound := map[string]map[string]any{} // by name; inners run concurrently
	inner := func(bound map[string]any) facade.Operator {
		mu.Lock()
		sawBound[str(bound["name"])] = bound
		mu.Unlock()
		if bound["name"] == "alpha" {
			return sliceOp{recs: []facade.Record{
				NewDocRecord(map[string]any{"encryption_status": "AES256"}),
			}}
		}
		return sliceOp{} // S(a)=∅ → left outer
	}

	join := NewBindJoin(1, outer,
		[]Binding{NewBinding("name", "name"), NewBinding("region", "region")}, inner, nil, 1)

	// Parallel fan-out: match tuples by name rather than emission order.
	byName := map[string]map[string]any{}
	for _, r := range collectDocs(t, join) {
		in, _ := r[KeyInput].(map[string]any)
		byName[str(in["name"])] = r
	}
	if len(byName) != 2 {
		t.Fatalf("got %d distinct rows, want 2: %v", len(byName), byName)
	}
	alpha := byName["alpha"]
	if out0, _ := alpha[KeyOutput].(map[string]any); out0["encryption_status"] != "AES256" {
		t.Errorf("alpha tuple = %v", alpha)
	}
	if in0, _ := alpha[KeyInput].(map[string]any); in0["region"] != "us-east-1" {
		t.Errorf("alpha input region = %v", alpha[KeyInput])
	}
	if beta := byName["beta"]; beta[KeyOutput] != nil {
		t.Errorf("beta tuple = %v, want output nil (left-outer)", beta)
	}
	if sawBound["alpha"]["region"] != "us-east-1" || sawBound["beta"]["name"] != "beta" {
		t.Errorf("bound slots not transferred: %v", sawBound)
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
