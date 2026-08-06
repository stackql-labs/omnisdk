package saga

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// listSource emits the given records once (a tiny operator so the tap has an upstream).
type listSource struct{ recs []facade.Record }

func (s listSource) Open(ctx context.Context) facade.Records {
	// Reuse a doc-record stream via a buffer.
	return recordsOf(ctx, s.recs)
}

// The tap must record one saga entry per record, capturing the declared undo/redo and keys, and
// forward the records unchanged.
func TestTapLogsUndoRedo(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithLog(context.Background(), NewWriter(&buf))

	src := listSource{recs: []facade.Record{
		bind.NewDocRecord(map[string]any{"vpc_id": "vpc-1"}),
		bind.NewDocRecord(map[string]any{"vpc_id": "vpc-2"}),
	}}
	op := NewTap(1, src, "CreateVpc", "CreateVpc", "DeleteVpc", []string{"vpc_id"}, 1)

	rs := op.Open(ctx)
	defer rs.Close()
	var forwarded int
	for rs.Next(ctx) {
		forwarded++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if forwarded != 2 {
		t.Fatalf("forwarded %d records, want 2 (tap must pass through)", forwarded)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("logged %d saga entries, want 2:\n%s", len(lines), buf.String())
	}
	var e facade.SagaEntry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("entry not JSON: %v", err)
	}
	if e.Redo != "CreateVpc" || e.Undo != "DeleteVpc" || e.Keys["vpc_id"] != "vpc-1" {
		t.Errorf("entry = %+v, want redo CreateVpc / undo DeleteVpc / vpc_id vpc-1", e)
	}
}

// helpers ---------------------------------------------------------------------

// recordsOf turns a fixed slice into a facade.Records cursor.
func recordsOf(ctx context.Context, recs []facade.Record) facade.Records {
	// A minimal single-use stream backed by an index.
	return &sliceCursor{recs: recs, i: -1}
}

type sliceCursor struct {
	recs []facade.Record
	i    int
}

func (c *sliceCursor) Next(context.Context) bool { c.i++; return c.i < len(c.recs) }
func (c *sliceCursor) Record() facade.Record     { return c.recs[c.i] }
func (c *sliceCursor) Err() error                { return nil }
func (c *sliceCursor) Close() error              { return nil }
