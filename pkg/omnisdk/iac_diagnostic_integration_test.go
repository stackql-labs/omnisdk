package omnisdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestIaCFailurePersistsDiagnosticWithoutHTTPLogging(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `<Response><Errors><Error><Code>RequestLimitExceeded</Code><Message>Rate exceeded</Message></Error></Errors><RequestID>request-test</RequestID></Response>`)
	}))
	defer server.Close()
	resources, err := omnisdk.ParseResources([]byte(`[{"key":"widget","provider":"widgets","address":"things.widgets","identity":"id","addressed_by":"id","desired":{"size":"S","color":"red"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	plan, err := omnisdk.Prepare(authRegistry, omnisdk.DesiredState(resources), omnisdk.NewLedger("diagnostic", state, "failed-run"), omnisdk.Args{Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := plan.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	rows.Close()
	bytes, err := os.ReadFile(filepath.Join(state, "journal", "failed-run.diagnostics.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(bytes)), "\n") {
		var entry struct {
			Key        string `json:"key"`
			Diagnostic struct {
				Event, Outcome string
				ProviderCode   string `json:"provider_code"`
				RequestID      string `json:"request_id"`
				Message        string
			} `json:"diagnostic"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Key == "diagnostic/widget" && entry.Diagnostic.Event == "failure" && entry.Diagnostic.ProviderCode == "RequestLimitExceeded" && entry.Diagnostic.RequestID == "request-test" && entry.Diagnostic.Message == "Rate exceeded" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing failure evidence: %s", bytes)
	}
}
