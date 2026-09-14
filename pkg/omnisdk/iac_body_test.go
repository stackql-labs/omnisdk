package omnisdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// A create against a JSON API sends the intent as the request BODY. Scattering its fields into the
// query is how a document meant for one API goes out shaped for another — and the provider reads it
// as a call with no content at all.
func TestGoogleCreateSendsTheIntentAsABody(t *testing.T) {
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))

	var body, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		body, path = string(b), r.URL.Path
		fmt.Fprint(w, `{"name":"op-1","targetId":"net-1"}`)
	}))
	defer srv.Close()

	res := []omnisdk.ManagedResource{
		omnisdk.NewResource("google/compute/network", "google", "compute.networks",
			[]byte(`{"name":"demo-net","autoCreateSubnetworks":false}`),
			map[string]string{"project": "demo"}, nil, "", "",
			// Client-named: the identity is what the caller asked for, so nothing has to be read back
			// out of an async Operation to know what was made.
			"", "network", ""),
	}
	pl, err := omnisdk.Converge(corpus, "bodydemo", t.TempDir(), "run-1", res, omnisdk.Args{
		Endpoint: srv.URL,
		Params:   map[string]string{"project": "demo"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for rows.Next() {
	}
	rows.Close()

	if body == "" {
		t.Fatalf("no request body was sent; path was %q", path)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("body %q is not JSON: %v", body, err)
	}
	if got["name"] != "demo-net" {
		t.Errorf("body = %s, want the intent document sent whole", body)
	}
	if strings.Contains(path, "autoCreateSubnetworks") {
		t.Errorf("path = %q, want intent fields in the body, not the query", path)
	}
}
