package endpoint_test

import (
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/endpoint"

	// The sdk package registers every service's real URL at init; importing it for side effects is
	// what makes this test exercise the ACTUAL registry rather than a fixture that can drift from it.
	_ "github.com/stackql-labs/omnisdk/internal/system_g/exchange/sdk"
)

// Real resolves every service to its registered default, {vars} expanded.
func TestRealResolvesRegisteredDefaults(t *testing.T) {
	e := endpoint.Real()
	if got := e.Resolve(endpoint.AWSS3, map[string]string{"region": "us-east-1"}); got != "https://s3.us-east-1.amazonaws.com" {
		t.Fatalf("aws.s3 = %q", got)
	}
	if got := e.Resolve(endpoint.GCPStorage, nil); got != "https://storage.googleapis.com/storage/v1" {
		t.Fatalf("gcp.storage = %q", got)
	}
	if len(e.Overridden()) != 0 {
		t.Fatalf("Real().Overridden() = %v, want none", e.Overridden())
	}
}

// Every service must be registered — an unregistered one resolves to "" and would silently produce a
// malformed request, so the enumerable set is the guard.
func TestEveryServiceIsRegistered(t *testing.T) {
	want := []string{
		endpoint.AWSS3, endpoint.AWSEC2, endpoint.AzureLogin, endpoint.AzureMgmt,
		endpoint.GCPOAuth, endpoint.GCPStorage, endpoint.GCPCRM, endpoint.GCPCompute,
	}
	for _, svc := range want {
		if endpoint.Default(svc) == "" {
			t.Fatalf("service %q has no registered default", svc)
		}
	}
	if len(endpoint.All()) != len(want) {
		t.Fatalf("All() = %v, want exactly %v", endpoint.All(), want)
	}
}

// A bare URL relocates every service by FRAGMENT — scheme/host/port replaced, each service's own
// registered path kept. That is what makes one mock able to mirror many providers' path shapes.
func TestUniformKeepsEachServicePath(t *testing.T) {
	e, err := endpoint.Parse("http://127.0.0.1:8085/")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Resolve(endpoint.GCPStorage, nil); got != "http://127.0.0.1:8085/storage/v1" {
		t.Fatalf("gcp.storage = %q, want the mock host with the service's own path", got)
	}
	if got := e.Resolve(endpoint.GCPCRM, nil); got != "http://127.0.0.1:8085/v3" {
		t.Fatalf("gcp.crm = %q", got)
	}
	if got := e.Resolve(endpoint.AWSS3, map[string]string{"region": "us-east-1"}); got != "http://127.0.0.1:8085" {
		t.Fatalf("aws.s3 = %q", got)
	}
	if !e.IsOverridden(endpoint.AzureMgmt) {
		t.Fatal("uniform must report every service overridden")
	}
}

// A mock mounted under a prefix keeps each service's path beneath it.
func TestUniformUnderAPrefix(t *testing.T) {
	e, _ := endpoint.Parse("http://127.0.0.1:8085/mock")
	if got := e.Resolve(endpoint.GCPStorage, nil); got != "http://127.0.0.1:8085/mock/storage/v1" {
		t.Fatalf("gcp.storage = %q", got)
	}
}

// A string value in the map replaces the WHOLE url — for a mock whose layout differs from the
// provider's rather than merely living elsewhere.
func TestWholeURLReplacement(t *testing.T) {
	e, err := endpoint.Parse(`{"gcp.storage":"http://localhost:9001/flat"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Resolve(endpoint.GCPStorage, nil); got != "http://localhost:9001/flat" {
		t.Fatalf("gcp.storage = %q, want the whole url replaced (no /storage/v1)", got)
	}
}

// An object value replaces only the named FRAGMENTS, leaving the rest of the registered URL intact —
// including the path, which is the point.
func TestFragmentReplacement(t *testing.T) {
	e, err := endpoint.Parse(`{"gcp.storage":{"scheme":"http","host":"localhost","port":"9001"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Resolve(endpoint.GCPStorage, nil); got != "http://localhost:9001/storage/v1" {
		t.Fatalf("gcp.storage = %q, want fragments replaced and the path kept", got)
	}
	// port alone, host alone, path alone
	e, _ = endpoint.Parse(`{"gcp.crm":{"port":"7000"}}`)
	if got := e.Resolve(endpoint.GCPCRM, nil); got != "https://cloudresourcemanager.googleapis.com:7000/v3" {
		t.Fatalf("port-only = %q", got)
	}
	e, _ = endpoint.Parse(`{"gcp.crm":{"path":"/v4"}}`)
	if got := e.Resolve(endpoint.GCPCRM, nil); got != "https://cloudresourcemanager.googleapis.com/v4" {
		t.Fatalf("path-only = %q", got)
	}
}

// Redirect one provider, leave another genuinely real — the case a single --endpoint cannot express.
func TestMappedLeavesUnnamedServicesReal(t *testing.T) {
	e, err := endpoint.Parse(`{"aws.s3":{"host":"localhost","port":"9000","scheme":"http"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Resolve(endpoint.AWSS3, map[string]string{"region": "us-east-1"}); got != "http://localhost:9000" {
		t.Fatalf("aws.s3 = %q", got)
	}
	if got := e.Resolve(endpoint.AzureMgmt, nil); got != "https://management.azure.com" {
		t.Fatalf("azure.mgmt = %q, want the real host", got)
	}
	if e.IsOverridden(endpoint.AzureMgmt) {
		t.Fatal("azure.mgmt must not report as overridden")
	}
	if got := strings.Join(e.Overridden(), ","); got != "aws.s3" {
		t.Fatalf("Overridden() = %q", got)
	}
}

// A typo'd service must fail loudly: ignoring it leaves a run the caller believes is mocked pointed
// at the real cloud — the exact failure this package exists to prevent.
func TestParseRejectsBadSpec(t *testing.T) {
	if _, err := endpoint.Parse(`{"aws.s4":"http://localhost:9000"}`); err == nil ||
		!strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("unknown service must error, got %v", err)
	}
	if _, err := endpoint.Parse(`{"aws.s3":`); err == nil {
		t.Fatal("malformed JSON must error")
	}
	if _, err := endpoint.Parse(`{"aws.s3":123}`); err == nil {
		t.Fatal("a non-url, non-fragment value must error")
	}
}

func TestParseEmptyIsReal(t *testing.T) {
	e, err := endpoint.Parse("   ")
	if err != nil || len(e.Overridden()) != 0 {
		t.Fatalf("Parse(blank) = %v, err=%v, want Real", e, err)
	}
}

// Registering a service twice is a programming error: two statements of where one service lives is
// the drift this package exists to prevent.
func TestDuplicateRegistrationPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("re-registering a service must panic")
		}
	}()
	endpoint.Register(endpoint.AWSS3, "https://example.invalid")
}
