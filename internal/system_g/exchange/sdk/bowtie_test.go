package sdk

import (
	"context"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

func collectDocs(t *testing.T, op facade.Operator) []map[string]any {
	t.Helper()
	ctx := context.Background()
	rs := op.Open(ctx)
	defer rs.Close()
	var out []map[string]any
	for rs.Next(ctx) {
		m, ok := bind.DocMap(rs.Record())
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

// explodeRows must turn one record carrying a projected []any into one record per row.
func TestExplodeRows(t *testing.T) {
	list := []any{
		map[string]any{"name": "a"},
		map[string]any{"name": "b"},
	}
	src := exchange.NewLiteralSource([]facade.Record{listRecord(list)}, 1)
	rows := collectDocs(t, exchange.NewExplodeRows(src, 1))
	if len(rows) != 2 || rows[0]["name"] != "a" || rows[1]["name"] != "b" {
		t.Fatalf("explode = %v, want two rows a,b", rows)
	}
}

// listRecord wraps a []any as an agnostic-list record (what a projection emits).
func listRecord(list []any) facade.Record {
	return record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewDocValue(list),
	})
}

// The sink flatten normalizes the raw output of a bind-join tuple onto the input row.
func TestS3EncryptionFlatten(t *testing.T) {
	flat := s3EncryptionFlatten()

	const sse = `<ServerSideEncryptionConfiguration><Rule>` +
		`<ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault>` +
		`<BucketKeyEnabled>true</BucketKeyEnabled></Rule></ServerSideEncryptionConfiguration>`

	tuple200 := bind.NewDocRecord(map[string]any{
		bind.KeyInput:  map[string]any{"name": "alpha"},
		bind.KeyOutput: map[string]any{"status": "200", "raw": sse},
	})
	ok, err := flat.Apply(tuple200)
	if err != nil {
		t.Fatalf("flatten 200: %v", err)
	}
	m, _ := bind.DocMap(ok)
	if m["name"] != "alpha" || m["encryption_status"] != "AES256" {
		t.Errorf("200 row = %v, want name alpha + AES256", m)
	}

	tuple404 := bind.NewDocRecord(map[string]any{
		bind.KeyInput:  map[string]any{"name": "beta"},
		bind.KeyOutput: map[string]any{"status": "404", "raw": `<Error/>`},
	})
	none, err := flat.Apply(tuple404)
	if err != nil {
		t.Fatalf("flatten 404: %v", err)
	}
	m, _ = bind.DocMap(none)
	if m["name"] != "beta" || m["encryption_status"] != "none" {
		t.Errorf("404 row = %v, want name beta + none", m)
	}
}
