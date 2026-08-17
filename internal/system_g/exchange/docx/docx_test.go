package docx_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/awsv4"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

const instancesXML = `<?xml version="1.0" encoding="UTF-8"?>
<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <reservationSet>
    <item>
      <instancesSet>
        <item><instanceId>i-aaa</instanceId><instanceType>t3.micro</instanceType></item>
        <item><instanceId>i-bbb</instanceId><instanceType>t3.small</instanceType></item>
      </instancesSet>
    </item>
  </reservationSet>
</DescribeInstancesResponse>`

// The document declares SigV4 for every call, so credentials are required to compile — supplying
// dummy ones here means the tests exercise the signing path rather than bypassing it.
var testCreds = docx.WithAWSCredentials(awsv4.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"})

func doc(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../../pkg/docparse/stackqldoc/testdata/aws/v00.00.00000/services/ec2.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// providerSecurity is what the AWS provider document declares for every service under it.
func providerSecurity(t *testing.T) aot.Security {
	t.Helper()
	b, err := os.ReadFile("../../../../pkg/docparse/stackqldoc/testdata/aws/v00.00.00000/provider.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := stackqldoc.ParseProvider(b)
	if err != nil {
		t.Fatal(err)
	}
	return p.Security()
}

func registry(t *testing.T) dsl.Registry {
	t.Helper()
	r, err := dsl.NewRegistry(gotemplate.Evaluators()...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// End to end: document bytes → a plan of one exchange plus the output exchange → rows. Every part of
// the call (verb, path, form body, response program, item path) came from the document.
func TestDocumentDrivenPlanListsInstances(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(instancesXML))
	}))
	defer srv.Close()

	p, err := docx.SelectPlan(doc(t), "instances",
		map[string]any{"region": "us-east-1"}, registry(t), docx.WithBaseURL(srv.URL), testCreds)
	if err != nil {
		t.Fatal(err)
	}

	rows := drain(t, p)
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want the two instances", rows)
	}
	if rows[0]["instanceId"] != "i-aaa" || rows[1]["instanceId"] != "i-bbb" {
		t.Fatalf("rows = %v", rows)
	}
	// the κ input travels onto the row, as for any hand-authored plan
	if rows[0]["region"] != "us-east-1" {
		t.Fatalf("bound input missing from row: %v", rows[0])
	}
	// the request is the one the DOCUMENT describes — verb and all. This document declares GET, so
	// asserting POST would be asserting the previous document rather than this one.
	if gotMethod != "GET" {
		t.Fatalf("method = %q", gotMethod)
	}
	_ = gotBody
}

// A non-2xx must fail loudly rather than read as an empty result.
func TestErrorResponseFailsLoudly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<Error><Code>AuthFailure</Code></Error>", http.StatusForbidden)
	}))
	defer srv.Close()

	p, err := docx.SelectPlan(doc(t), "instances",
		map[string]any{"region": "us-east-1"}, registry(t), docx.WithBaseURL(srv.URL), testCreds)
	if err != nil {
		t.Fatal(err)
	}
	op := plan.ComposeRows(1, p)
	recs := op.Open(context.Background())
	defer recs.Close()
	for recs.Next(context.Background()) {
	}
	if recs.Err() == nil {
		t.Fatal("a 403 must surface as an error")
	}
}

// Scope the document declares is required and never inferred — the server variable has a default in
// the document, and using it would be an invented answer.
func TestMissingInputIsRejected(t *testing.T) {
	_, err := docx.SelectPlan(doc(t), "instances", nil, registry(t), testCreds)
	if err == nil || !strings.Contains(err.Error(), `requires input "region"`) {
		t.Fatalf("err = %v", err)
	}
}

// A document that declares signing must not silently send an unsigned request.
func TestDeclaredSigningIsRequired(t *testing.T) {
	// this service document inherits its scheme from the provider, so signing is asserted through the
	// provider-level declaration rather than a service-level one
	sec := providerSecurity(t)
	_, err := docx.SelectPlan(doc(t), "instances", map[string]any{"region": "us-east-1"}, registry(t),
		docx.WithProviderSecurity(sec))
	if err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Fatalf("err = %v", err)
	}
	// ...and the request really is signed when they are supplied
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(instancesXML))
	}))
	defer srv.Close()
	p, err := docx.SelectPlan(doc(t), "instances", map[string]any{"region": "us-east-1"},
		registry(t), docx.WithBaseURL(srv.URL), testCreds, docx.WithProviderSecurity(sec))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, p)
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("Authorization = %q, want a SigV4 signature", auth)
	}
	// the credential scope names the service the document's own host implies
	if !strings.Contains(auth, "/us-east-1/ec2/aws4_request") {
		t.Fatalf("credential scope = %q, want region/service from the document", auth)
	}
}

// A document naming a language the registry does not implement must say so, naming both.
func TestUnsupportedEvaluatorIsRejected(t *testing.T) {
	empty, err := dsl.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	_, err = docx.SelectPlan(doc(t), "instances", map[string]any{"region": "us-east-1"}, empty, testCreds)
	if err == nil || !strings.Contains(err.Error(), "needs evaluator") {
		t.Fatalf("err = %v", err)
	}
}

func drain(t *testing.T, p plan.Plan) []map[string]any {
	t.Helper()
	ctx := context.Background()
	recs := plan.ComposeRows(1, p).Open(ctx)
	defer recs.Close()
	var out []map[string]any
	for recs.Next(ctx) {
		m, _ := bind.DocMap(recs.Record())
		out = append(out, m)
	}
	if err := recs.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}
