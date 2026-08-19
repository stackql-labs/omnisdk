package stackqldoc_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

func catalog(t *testing.T) aot.Catalog {
	t.Helper()
	c, err := stackqldoc.Open(os.DirFS("testdata/aws/v00.00.00000"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The bundle resolves to addresses. Only services whose document is actually present are addressable
// — the provider lists hundreds, and claiming those would be claiming something untrue.
func TestBundleResolvesToAddresses(t *testing.T) {
	c := catalog(t)
	// The committed fixture is a SUBSET of the real bundle: the full corpus is hundreds of megabytes
	// and a module zip carries testdata, so only the services these tests exercise are kept.
	svcs := c.Services()
	if len(svcs) == 0 {
		t.Fatal("Services() = none")
	}
	if len(svcs) > len(c.Provider().Services()) {
		t.Fatal("a bundle cannot make more services addressable than the provider lists")
	}

	// One address per resource with an operation-backed SELECT. Pinning an exact count would pin the
	// bundle's contents rather than the resolution, and bundles are refreshed.
	paths := c.Paths()
	if len(paths) < 50 {
		t.Fatalf("addresses = %d, far fewer than this fixture declares", len(paths))
	}
	for i := 1; i < len(paths); i++ {
		if paths[i-1] > paths[i] {
			t.Fatalf("paths not sorted at %d: %q then %q", i, paths[i-1], paths[i])
		}
	}
	found := map[string]bool{}
	for _, p := range paths {
		found[p] = true
		if !strings.HasPrefix(p, "stackql_unstable_aws.") {
			t.Fatalf("unexpected address %q", p)
		}
	}
	for _, want := range []string{"stackql_unstable_aws.ec2.instances", "stackql_unstable_aws.ec2.volumes", "stackql_unstable_aws.ec2.vpcs"} {
		if !found[want] {
			t.Fatalf("%s missing from %d addresses", want, len(paths))
		}
	}
}

// An address resolves to the same exchange the service document yields directly.
func TestAddressResolvesToExchange(t *testing.T) {
	ex, err := catalog(t).Exchange("stackql_unstable_aws.ec2.instances")
	if err != nil {
		t.Fatal(err)
	}
	if ex.OperationID() != "GET_DescribeInstances" {
		t.Fatalf("OperationID() = %q", ex.OperationID())
	}
	// This document states no service-level scheme: auth is declared once, on the provider, and a
	// silent service document inherits it rather than opting out.
	if ex.Security().Scheme() != aot.SchemeNone {
		t.Fatalf("Scheme() = %q, want the service document to defer to the provider", ex.Security().Scheme())
	}
}

// Every failure says which part of the address was wrong.
func TestAddressErrorsAreSpecific(t *testing.T) {
	c := catalog(t)
	for _, tc := range []struct{ addr, want string }{
		{"stackql_unstable_aws.ec2", "not <provider>.<service>.<resource>"},
		{"gcp.ec2.instances", "not for provider"},
		{"stackql_unstable_aws.nosuchservice.things", "no document in this bundle"},
		{"stackql_unstable_aws.ec2.no_such_resource", "no resource"},
	} {
		_, err := c.Exchange(tc.addr)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("Exchange(%q) = %v, want %q", tc.addr, err, tc.want)
		}
	}
}

// The namespace is a caller's decision, not a constant. A consumer that has decided the documents are
// authoritative can drop or rename it without this package caring.
func TestProviderPrefixIsConfigurable(t *testing.T) {
	for _, tc := range []struct {
		opts []stackqldoc.Option
		want string
	}{
		{nil, "stackql_unstable_aws.ec2.instances"},
		{[]stackqldoc.Option{stackqldoc.WithProviderPrefix("preview_")}, "preview_aws.ec2.instances"},
		{[]stackqldoc.Option{stackqldoc.WithProviderPrefix("")}, "aws.ec2.instances"},
	} {
		c, err := stackqldoc.Open(os.DirFS("testdata/aws/v00.00.00000"), tc.opts...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exchange(tc.want); err != nil {
			t.Fatalf("Exchange(%q) = %v", tc.want, err)
		}
	}
}
