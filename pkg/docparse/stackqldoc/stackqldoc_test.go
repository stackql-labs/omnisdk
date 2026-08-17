package stackqldoc_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

func load(t *testing.T) stackqldoc.Doc {
	t.Helper()
	b, err := os.ReadFile("testdata/aws/v00.00.00000/services/ec2.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d, err := stackqldoc.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The whole point: document bytes in, a runnable exchange out. Everything asserted here was RESOLVED
// from the document by following its own $refs — none of it is knowledge this package holds about EC2.
func TestSelectInstancesResolvesFromDocument(t *testing.T) {
	ex, err := load(t).Select("instances")
	if err != nil {
		t.Fatal(err)
	}
	// the exchange is named for the METHOD that backs SELECT, since a resource may bind several
	if ex.Name() != "describe_instances" {
		t.Fatalf("Name() = %q", ex.Name())
	}
	// the server URL's {region} variable becomes a bound input — scope the caller supplies, never
	// something the parser invents
	if got := strings.Join(ex.Inputs(), ","); got != "region" {
		t.Fatalf("Inputs() = %q, want region", got)
	}

	// sqlVerbs.select → methods/describe → #/paths/...DescribeInstances.../post
	if ex.OperationID() != "GET_DescribeInstances" {
		t.Fatalf("OperationID() = %q, want the operation SELECT points at", ex.OperationID())
	}
	if ex.Response().ObjectKey() != "$.line_items" {
		t.Fatalf("ObjectKey() = %q", ex.Response().ObjectKey())
	}
}

// The document attaches its response transform to the SOURCE. An AOT exchange reports it as a
// declaration — an evaluator name and a program — and does not run it or decide where it runs.
func TestResponseTransformIsDeclaredNotApplied(t *testing.T) {
	ex, err := load(t).Select("instances")
	if err != nil {
		t.Fatal(err)
	}
	resp := ex.Response()
	if resp.MediaType() != "application/xml" || resp.OverrideMediaType() != "application/json" {
		t.Fatalf("media types = %q → %q", resp.MediaType(), resp.OverrideMediaType())
	}
	tr := resp.Transform()
	if tr.Type() != "golang_template_mxj_v0.3.0" {
		t.Fatalf("Transform().Type() = %q", tr.Type())
	}
	if !strings.Contains(tr.Body(), "line_items") {
		t.Fatalf("Transform().Body() does not look like the declared program: %.80q", tr.Body())
	}
	// ObjectKey addresses the TRANSFORMED body, which is why the two travel together
	if !strings.Contains(resp.ObjectKey(), "line_items") {
		t.Fatalf("ObjectKey %q should address the transform's output", resp.ObjectKey())
	}
}

// Pagination is read from the document when it declares any; this operation declares none, and
// reporting that faithfully is the point — an invented default would page a call that does not page.
func TestPaginationIsDeclared(t *testing.T) {
	ex, err := load(t).Select("instances")
	if err != nil {
		t.Fatal(err)
	}
	if p := ex.Response().Pagination(); p.Declared() {
		k, loc := p.ResponseToken()
		t.Fatalf("expected none declared, got %q/%q", k, loc)
	}
}

// The path key encodes the operation's identity as __-prefixed pseudo-parameters. They are not query
// parameters — with a form request they are the body naming the action, which is what makes the
// resolved request the call EC2 actually accepts.
func TestResolvedRequestIsTheRealCall(t *testing.T) {
	ex, err := load(t).Select("instances")
	if err != nil {
		t.Fatal(err)
	}
	req := ex.Request()
	if req.Method() != "GET" {
		t.Fatalf("Method = %q", req.Method())
	}
	// {region} survives as a template — a binder substitutes it from the bound row
	if !strings.HasPrefix(req.URL(), "https://ec2.{region}.amazonaws.com/") {
		t.Fatalf("URL = %q", req.URL())
	}
	// this document states the action as ordinary query parameters, so they belong in the URL. An
	// earlier one marked them with a __ prefix, meaning "not a query parameter" — the difference is
	// the document's to make, and reading it wrongly sends a different call.
	if !strings.Contains(req.URL(), "Action=DescribeInstances") ||
		!strings.Contains(req.URL(), "Version=2016-11-15") {
		t.Fatalf("URL lost the action: %q", req.URL())
	}
	// no __-prefixed marker may survive into the wire request
	if strings.Contains(req.URL(), "__") {
		t.Fatalf("pseudo-parameter leaked into the URL: %q", req.URL())
	}
	for k := range req.Params() {
		if strings.HasPrefix(k, "__") {
			t.Fatalf("pseudo-parameter %q leaked into the request", k)
		}
	}
}

func TestResourcesAreDiscoverable(t *testing.T) {
	names := load(t).Resources()
	if len(names) < 2 {
		t.Fatalf("Resources() = %v", names)
	}
	found := false
	for _, n := range names {
		if n == "instances" {
			found = true
		}
	}
	if !found {
		t.Fatalf("instances missing from %d resources", len(names))
	}
}

// A resource with no SELECT is a real answer about the provider, not an empty result.
func TestErrorsAreSpecific(t *testing.T) {
	d := load(t)
	if _, err := d.Select("no_such_resource"); err == nil ||
		!strings.Contains(err.Error(), "no resource") {
		t.Fatalf("unknown resource error = %v", err)
	}
	if _, err := stackqldoc.Parse([]byte("openapi: 3.0.0\n")); err == nil ||
		!strings.Contains(err.Error(), "no x-stackQL-resources") {
		t.Fatalf("a document with no resources must say so, got %v", err)
	}
	if _, err := stackqldoc.Parse([]byte("\tnot: yaml: [")); err == nil {
		t.Fatal("malformed yaml must error")
	}
}
