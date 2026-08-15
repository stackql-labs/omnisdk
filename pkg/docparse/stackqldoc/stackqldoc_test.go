package stackqldoc_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

func load(t *testing.T) stackqldoc.Doc {
	t.Helper()
	b, err := os.ReadFile("testdata/ec2.yaml")
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
	if ex.Name() != "instances" {
		t.Fatalf("Name() = %q", ex.Name())
	}
	// the server URL's {region} variable becomes a bound input — scope the caller supplies, never
	// something the parser invents
	if got := strings.Join(ex.In(), ","); got != "region" {
		t.Fatalf("In() = %q, want region", got)
	}

	type resolved interface {
		OperationID() string
		ObjectKey() string
	}
	r, ok := ex.(resolved)
	if !ok {
		t.Fatal("exchange does not expose what it resolved")
	}
	// sqlVerbs.select → methods/describe → #/paths/...DescribeInstances.../post
	if r.OperationID() != "GET_DescribeInstances" {
		t.Fatalf("OperationID() = %q, want the operation SELECT points at", r.OperationID())
	}
	if r.ObjectKey() != "$.line_items" {
		t.Fatalf("ObjectKey() = %q", r.ObjectKey())
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
	req := ex.(interface{ Request() httpx.Request }).Request()
	if req.Method != "POST" {
		t.Fatalf("Method = %q", req.Method)
	}
	// {region} survives as a template — the engine substitutes it from the bound row
	if req.URL != "https://ec2.{region}.amazonaws.com/" {
		t.Fatalf("URL = %q", req.URL)
	}
	if got := req.Body.Params["Action"]; got != "DescribeInstances" {
		t.Fatalf("body Action = %v, want the action lifted out of the path key", got)
	}
	if got := req.Body.Params["Version"]; got != "2016-11-15" {
		t.Fatalf("body Version = %v", got)
	}
	// no __-prefixed marker may survive into the wire request
	for k := range req.Body.Params {
		if strings.HasPrefix(k, "__") {
			t.Fatalf("pseudo-parameter %q leaked into the request", k)
		}
	}
	if strings.Contains(req.URL, "__") {
		t.Fatalf("pseudo-parameter leaked into the URL: %q", req.URL)
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
