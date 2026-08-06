package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"

	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
)

// The egress transforms run inside the output exchange: response XML → agnostic → projection
// → JSONL. Two buckets in, two JSONL objects out, each with name/created. Send points at a
// local server, so no real S3.
func TestBeetleOutputRendersJSONL(t *testing.T) {
	const xml = `<ListAllMyBucketsResult><Buckets>` +
		`<Bucket><Name>alpha</Name><CreationDate>2019-01-01T00:00:00Z</CreationDate></Bucket>` +
		`<Bucket><Name>beta</Name><CreationDate>2020-02-02T00:00:00Z</CreationDate></Bucket>` +
		`</Buckets></ListAllMyBucketsResult>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(xml))
	}))
	defer srv.Close()

	var out bytes.Buffer
	seed := exchange.NewLiteralSource([]facade.Record{httpx.NewRequestRecord("GET", srv.URL+"/", nil, nil)}, 1)
	signed := exchange.NewTransformExchange(1, seed, NewSigV4Transform(testSigner()), 1)
	send := httpx.NewSendExchange(2, signed, srv.Client(), facade.FormRead, 1)
	egress := []facade.Transform{
		transform.NewXMLToAgnostic(),
		transform.NewProjection("ListAllMyBucketsResult.Buckets.Bucket", []transform.Column{
			{Out: "name", Path: "Name"},
			{Out: "created", Path: "CreationDate"},
		}),
	}
	sink := plan.NewOutputExchange(3, send, egress, encoder.NewJSONLEncoder(), &out)

	ctx := context.Background()
	rs := sink.Open(ctx)
	defer rs.Close()
	for rs.Next(ctx) { //nolint // sink emits nothing; ranging drives the pull
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d JSONL lines, want 2:\n%s", len(lines), out.String())
	}
	want := []map[string]string{
		{"name": "alpha", "created": "2019-01-01T00:00:00Z"},
		{"name": "beta", "created": "2020-02-02T00:00:00Z"},
	}
	for i, line := range lines {
		var got map[string]string
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d not JSON (%v): %q", i, err, line)
		}
		if got["name"] != want[i]["name"] || got["created"] != want[i]["created"] {
			t.Errorf("line %d = %v, want %v", i, got, want[i])
		}
	}
}

// The full beetle chain: literalSource → transform(sigv4) → send → output(sink). Ranging
// the sink to EOF drives the pull; the request must flow through sign carrying the SigV4
// headers to the wire, and the response body must land on the output writer.
func TestBeetleGraphSignsSendsOutputs(t *testing.T) {
	const bodyXML = `<ListAllMyBucketsResult><Buckets/></ListAllMyBucketsResult>`

	var gotAuth, gotDate string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDate = r.Header.Get("X-Amz-Date")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(bodyXML))
	}))
	defer srv.Close()

	var out bytes.Buffer
	seed := exchange.NewLiteralSource([]facade.Record{httpx.NewRequestRecord("GET", srv.URL+"/", nil, nil)}, 1)
	signed := exchange.NewTransformExchange(1, seed, NewSigV4Transform(testSigner()), 1)
	send := httpx.NewSendExchange(2, signed, srv.Client(), facade.FormRead, 1)
	sink := plan.NewOutputExchange(3, send, nil, encoder.NewAnonymousEncoder(), &out)

	ctx := context.Background()
	rs := sink.Open(ctx)
	defer rs.Close()
	for rs.Next(ctx) { //nolint // sink emits nothing; ranging drives the pull
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}

	if out.String() != bodyXML {
		t.Errorf("output = %q, want %q", out.String(), bodyXML)
	}
	// The signature survived: record → transform → record → send → wire.
	for _, want := range []string{"AWS4-HMAC-SHA256", "Credential=AK/", "/us-east-1/s3/aws4_request"} {
		if !strings.Contains(gotAuth, want) {
			t.Errorf("Authorization missing %q: %q", want, gotAuth)
		}
	}
	if gotDate == "" {
		t.Error("X-Amz-Date did not reach the wire")
	}
}
