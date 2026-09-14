package namespace_test

import (
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/namespace"
)

// A bare identifier means a column of the result. These exchanges answer questions, so the
// projection outranks every request space by default.
func TestProjectionWinsABareName(t *testing.T) {
	ns, err := namespace.Build([]namespace.Attribute{
		namespace.New(namespace.Projection, "VpcId"),
		namespace.New(namespace.Query, "VpcId"),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, err := ns.Resolve("VpcId")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Space() != namespace.Projection {
		t.Errorf("VpcId resolved to %s, want the projection", got.Space())
	}
	// The shadowed one is still reachable; precedence hides a name, it does not remove it.
	shadowed, err := ns.Resolve("query.VpcId")
	if err != nil || shadowed.Space() != namespace.Query {
		t.Errorf("query.VpcId = %v (%v), want the query parameter", shadowed, err)
	}
}

// Request spaces have no claim over one another, so a name two of them declare stays ambiguous. A
// ranking invented here would resolve something only the caller can settle.
func TestRequestSpacesAreIncomparable(t *testing.T) {
	_, err := namespace.Build([]namespace.Attribute{
		namespace.New(namespace.Query, "filter"),
		namespace.New(namespace.Body, "filter"),
	})
	if err == nil {
		t.Fatal("a name claimed by query and body was accepted, want an ambiguity")
	}
	if !strings.Contains(err.Error(), "body and query") {
		t.Errorf("error = %v, want it to name both claimants", err)
	}
}

// Ambiguity is reported when the namespace is BUILT, not when a name is looked up: a name nobody
// happens to read is still ambiguous, and finding out per query makes the fault depend on the query.
func TestAmbiguityIsReportedUpFront(t *testing.T) {
	_, err := namespace.Build([]namespace.Attribute{
		namespace.New(namespace.Query, "a"),
		namespace.New(namespace.Body, "a"),
		namespace.New(namespace.Header, "untouched"),
	})
	if err == nil {
		t.Fatal("build succeeded with an ambiguous name")
	}
}

// Two attributes in one space with one name cannot be separated by any ranking.
func TestDuplicateWithinASpaceIsAlwaysAmbiguous(t *testing.T) {
	if _, err := namespace.Build([]namespace.Attribute{
		namespace.New(namespace.Query, "dup"),
		namespace.New(namespace.Query, "dup"),
	}, namespace.WithPrecedence(namespace.RankedOrder(namespace.Query, namespace.Body))); err == nil {
		t.Error("a duplicate within one space was accepted")
	}
}

// A caller may read a document differently; what must not change is the reading within one run.
func TestPrecedenceIsConfigurable(t *testing.T) {
	ns, err := namespace.Build([]namespace.Attribute{
		namespace.New(namespace.Projection, "name"),
		namespace.New(namespace.Body, "name"),
	}, namespace.WithPrecedence(namespace.RankedOrder(namespace.Body, namespace.Projection)))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, err := ns.Resolve("name")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Space() != namespace.Body {
		t.Errorf("name resolved to %s, want the body under a caller's ranking", got.Space())
	}
}

// A space outside the ranking is incomparable with everything, so it cannot silently win.
func TestUnrankedSpaceStaysAmbiguous(t *testing.T) {
	if _, err := namespace.Build([]namespace.Attribute{
		namespace.New(namespace.Header, "x"),
		namespace.New(namespace.Body, "x"),
	}, namespace.WithPrecedence(namespace.RankedOrder(namespace.Projection, namespace.Query))); err == nil {
		t.Error("two unranked spaces resolved a shared name")
	}
}

func TestUnknownNames(t *testing.T) {
	ns, err := namespace.Build([]namespace.Attribute{namespace.New(namespace.Query, "known")})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := ns.Resolve("absent"); err == nil {
		t.Error("an unknown bare name resolved")
	}
	if _, err := ns.Resolve("body.known"); err == nil {
		t.Error("a qualified name in the wrong space resolved")
	}
}

func TestListing(t *testing.T) {
	ns, err := namespace.Build([]namespace.Attribute{
		namespace.New(namespace.Query, "b"),
		namespace.New(namespace.Query, "a"),
		namespace.New(namespace.Path, "p"),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	in := ns.In(namespace.Query)
	if len(in) != 2 || in[0].Name() != "a" {
		t.Errorf("In(query) = %v, want sorted a,b", in)
	}
	if spaces := ns.Spaces(); len(spaces) != 2 {
		t.Errorf("Spaces() = %v, want path and query", spaces)
	}
}
