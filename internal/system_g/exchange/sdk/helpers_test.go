package sdk

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// testSigner is the shared SigV4 signer for tests (us-east-1/s3, dummy creds).
func testSigner() Signer {
	return NewSigV4Signer("us-east-1", "s3", Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}, true)
}

// readValue reads a record value as a string, failing the test if absent.
func readValue(t *testing.T, rec facade.Record, key string) string {
	t.Helper()
	v := rec.Get(key)
	if v == nil {
		t.Fatalf("record missing %q", key)
	}
	b, err := io.ReadAll(v.Reader())
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return string(b)
}

// testCreds is the dummy AWS credential set for exchange tests.
func testCreds() Credentials {
	return Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}
}

// S3 ListBuckets now runs on httpx: the XML <ContinuationToken> drives pagination through the
// injected mxj decoder — proving XML composes into httpx's continuation with no encoding welded
// in. Page 1 carries a token, page 2 does not; both pages must be emitted, both must send
// max-buckets, and page 2 must carry the token page 1 returned.
func TestS3ListBucketsPaginatesXMLOnHTTPX(t *testing.T) {
	const page1 = `<?xml version="1.0"?><ListAllMyBucketsResult>` +
		`<Buckets><Bucket><Name>a</Name></Bucket></Buckets>` +
		`<ContinuationToken>TOK</ContinuationToken></ListAllMyBucketsResult>`
	const page2 = `<?xml version="1.0"?><ListAllMyBucketsResult>` +
		`<Buckets><Bucket><Name>b</Name></Bucket></Buckets></ListAllMyBucketsResult>`

	var gotTokens []string
	var gotMaxBuckets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTokens = append(gotTokens, r.URL.Query().Get("continuation-token"))
		gotMaxBuckets = append(gotMaxBuckets, r.URL.Query().Get("max-buckets"))
		if r.URL.Query().Get("continuation-token") == "" {
			_, _ = io.WriteString(w, page1)
			return
		}
		_, _ = io.WriteString(w, page2)
	}))
	defer srv.Close()

	op := NewS3ListBuckets(0, "us-east-1", testCreds(), srv.URL, 1000)
	ctx := context.Background()
	rs := op.Open(ctx)
	defer rs.Close()
	var pages int
	for rs.Next(ctx) {
		pages++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}

	if pages != 2 {
		t.Fatalf("emitted %d pages, want 2 (token then exhaustion)", pages)
	}
	if len(gotTokens) != 2 || gotTokens[0] != "" || gotTokens[1] != "TOK" {
		t.Errorf("continuation-token sequence = %v, want [\"\", \"TOK\"]", gotTokens)
	}
	for i, mb := range gotMaxBuckets {
		if mb != "1000" {
			t.Errorf("page %d max-buckets = %q, want 1000", i, mb)
		}
	}
}
