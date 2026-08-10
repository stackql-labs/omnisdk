package omnisdk_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// Discovery: a consumer lists resources (regex-filtered), gets one's metadata, then runs it — no
// knowledge of the engine or transport. This drives the catalog + the azure resource end to end.
func TestFacadeCatalogAndRun(t *testing.T) {
	// list resources with a regex filter (package convenience over the default Catalog)
	got, err := omnisdk.Resources("storage")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, r := range got {
		paths[r.Path] = true
	}
	if !paths["azure.storage.accounts"] || !paths["google.storage.buckets"] {
		t.Fatalf("resources(storage) = %v, want azure + google storage resources", got)
	}

	// a resource carries a canonical JSON Schema and no input params
	az, ok := omnisdk.GetResource("azure.storage.accounts")
	if !ok || az.Schema["$schema"] == nil || az.Schema["properties"] == nil {
		t.Fatalf("GetResource(azure.storage.accounts) = %+v, ok=%v", az, ok)
	}

	// methods hang off a resource, each with a signature (params + output schema)
	ms, err := omnisdk.Methods("azure.storage.accounts")
	if err != nil || len(ms) == 0 || ms[0].Path != "azure.storage.accounts.list" {
		t.Fatalf("Methods(azure.storage.accounts) = %v, err=%v", ms, err)
	}
	props, _ := ms[0].Schema["properties"].(map[string]any)
	if props["encryption_class"] == nil {
		t.Fatalf("method output schema missing encryption_class: %+v", ms[0].Schema)
	}

	// run it against a stub Azure ARM
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/subscriptions":
			_, _ = w.Write([]byte(`{"value":[{"subscriptionId":"sub1"}]}`))
		case strings.Contains(r.URL.Path, "/providers/Microsoft.Storage/storageAccounts"):
			_, _ = w.Write([]byte(`{"value":[{"name":"s1","properties":{` +
				`"encryption":{"keySource":"Microsoft.Keyvault"},` +
				`"allowBlobPublicAccess":true,"supportsHttpsTrafficOnly":true}}]}`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("OMNISDK_TEST_TOK", "mock-token")

	pl, err := omnisdk.New("azure.storage.accounts.list", omnisdk.Args{
		Endpoint: srv.URL,
		Auth:     &omnisdk.Auth{Type: "bearer", CredentialsEnvVar: "OMNISDK_TEST_TOK"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var out []omnisdk.Row
	for rows.Next() {
		out = append(out, rows.Row())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(out) != 1 || out[0]["provider"] != "azure" || out[0]["encryption_class"] != "customer-managed" {
		t.Fatalf("rows = %v", out)
	}
}

func TestFacadeUnknownMethod(t *testing.T) {
	if _, err := omnisdk.New("nope.nope.nope", omnisdk.Args{}); err == nil {
		t.Fatal("unknown method should error")
	}
}

// Scope params are enforced by New — never inferred (here: exactly one of project/org).
func TestFacadeRequiresScopeParam(t *testing.T) {
	if _, err := omnisdk.New("google.storage.buckets.list", omnisdk.Args{}); err == nil {
		t.Fatal("google.storage.buckets.list with no project/org should error")
	}
}

// The cross-cloud composite is discoverable and planned exactly like any other method: one resource,
// one method path, the same canonical blob schema — the consumer never assembles the member list.
func TestFacadeCrossCloudComposite(t *testing.T) {
	r, ok := omnisdk.GetResource("omni.storage.buckets")
	if !ok || r.Schema["properties"] == nil {
		t.Fatalf("GetResource(omni.storage.buckets) = %+v, ok=%v", r, ok)
	}
	ms, err := omnisdk.Methods("omni.storage.buckets")
	if err != nil || len(ms) != 1 || ms[0].Path != "omni.storage.buckets.list" {
		t.Fatalf("Methods(omni.storage.buckets) = %v, err=%v", ms, err)
	}
	// the composite publishes the uniform blob schema its legs already normalize to
	props, _ := ms[0].Schema["properties"].(map[string]any)
	if props["provider"] == nil || props["encryption_class"] == nil {
		t.Fatalf("composite schema is not the blob schema: %+v", ms[0].Schema)
	}

	// its own required params are enforced up front...
	if _, err := omnisdk.New("omni.storage.buckets.list", omnisdk.Args{
		Params: map[string]string{"project": "p"},
	}); err == nil {
		t.Fatal("composite without region should error")
	}
	// ...and its exactly-one rule is declared, not hand-rolled in a plan builder: enforced before any
	// leg is built, so no credentials are needed to reach it, and reported in the method's own names.
	_, err = omnisdk.New("omni.storage.buckets.list", omnisdk.Args{
		Params: map[string]string{"region": "us-east-1"},
	})
	if err == nil || !strings.Contains(err.Error(), "'google_project', 'google_org'") {
		t.Fatalf("composite should report its own exactly-one rule, got %v", err)
	}
	// supplying BOTH alternatives is just as wrong as supplying neither
	_, err = omnisdk.New("omni.storage.buckets.list", omnisdk.Args{
		Params: map[string]string{"region": "us-east-1", "google_project": "p", "google_org": "o"},
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("composite should reject both alternatives at once, got %v", err)
	}
}

// The exactly-one rule is part of the published signature, so a consumer can see it without running.
func TestFacadeExactlyOneIsDiscoverable(t *testing.T) {
	m, ok := omnisdk.GetMethod("google.storage.buckets.list")
	if !ok || len(m.ExactlyOne) != 1 {
		t.Fatalf("GetMethod(google.storage.buckets.list).ExactlyOne = %v, ok=%v", m.ExactlyOne, ok)
	}
	if got := strings.Join(m.ExactlyOne[0], ","); got != "google_project,google_org" {
		t.Fatalf("ExactlyOne group = %q, want google_project,google_org", got)
	}
	c, _ := omnisdk.GetMethod("omni.storage.buckets.list")
	if got := strings.Join(c.ExactlyOne[0], ","); got != "google_project,google_org" {
		t.Fatalf("composite ExactlyOne group = %q, want the same names as its leg", got)
	}
}
