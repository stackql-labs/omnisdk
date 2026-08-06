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
