package stackqldoc_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

func provider(t *testing.T) aot.Provider {
	t.Helper()
	b, err := os.ReadFile("testdata/aws/v00.00.00000/provider.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := stackqldoc.ParseProvider(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The provider document is the catalogue: which services exist and where each one's document lives.
// That is how a service is discovered rather than known.
func TestProviderServicesAreDiscoverable(t *testing.T) {
	p := provider(t)
	if p.Name() != "aws" || p.Version() == "" {
		t.Fatalf("provider = %q %q", p.Name(), p.Version())
	}
	svcs := p.Services()
	if len(svcs) == 0 {
		t.Fatal("the provider lists no services")
	}
	// sorted, so a caller can present them without re-sorting
	for i := 1; i < len(svcs); i++ {
		if svcs[i-1].Name() > svcs[i].Name() {
			t.Fatalf("services not sorted at %d: %q then %q", i, svcs[i-1].Name(), svcs[i].Name())
		}
	}
	ec2, ok := p.Service("ec2")
	if !ok {
		t.Fatal("ec2 missing from the provider catalogue")
	}
	if ec2.Name() != "ec2" || ec2.Version() == "" {
		t.Fatalf("ec2 = %+v", ec2)
	}
	// the ref is how the service document is located — the last segment matches the file on disk
	if !strings.HasSuffix(ec2.Ref(), "services/ec2.yaml") {
		t.Fatalf("ec2 ref = %q", ec2.Ref())
	}
	if _, ok := p.Service("no_such_service"); ok {
		t.Fatal("unknown service must not resolve")
	}
}

// The signing algorithm is declared ONCE, on the provider — not re-derived per service document.
func TestSigningAlgorithmComesFromTheProvider(t *testing.T) {
	p := provider(t)
	sec := p.Security()
	if sec.Scheme() != aot.SchemeAWSSigV4 {
		t.Fatalf("Scheme() = %q, want the provider's declared aws_signing_v4", sec.Scheme())
	}
	if sec.Name() != "aws_signing_v4" {
		t.Fatalf("Name() = %q, want the document's own spelling", sec.Name())
	}
	// the document names where credentials live; it never carries their values
	creds := p.Credentials()
	if creds.KeyIDEnvVar() != "AWS_ACCESS_KEY_ID" || creds.SecretEnvVar() != "AWS_SECRET_ACCESS_KEY" {
		t.Fatalf("credentials = %q / %q", creds.KeyIDEnvVar(), creds.SecretEnvVar())
	}
}

// A service document is not a provider document, and saying so beats parsing it into nonsense.
func TestNonProviderDocumentIsRejected(t *testing.T) {
	if _, err := stackqldoc.ParseProvider([]byte("openapi: 3.0.0\npaths: {}\n")); err == nil ||
		!strings.Contains(err.Error(), "not a provider document") {
		t.Fatalf("err = %v", err)
	}
}
