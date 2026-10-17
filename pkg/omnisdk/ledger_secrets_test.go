package omnisdk_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// secretIn reports whether secret appears anywhere under dir, in a file as written or in any string
// field of a JSON file decoded from base64 — the ledger keeps intent that way.
func secretIn(t *testing.T, dir, secret string) []string {
	t.Helper()
	var found []string
	var scan func(path string, v any)
	scan = func(path string, v any) {
		switch x := v.(type) {
		case string:
			if b, err := base64.StdEncoding.DecodeString(x); err == nil && strings.Contains(string(b), secret) {
				found = append(found, path)
			}
		case map[string]any:
			for _, e := range x {
				scan(path, e)
			}
		case []any:
			for _, e := range x {
				scan(path, e)
			}
		}
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), secret) {
			found = append(found, path)
		}
		for _, line := range strings.Split(string(b), "\n") {
			var v any
			if json.Unmarshal([]byte(line), &v) == nil {
				scan(path, v)
			}
		}
		return nil
	})
	return found
}

// A converging run records intent in the ledger and journal before each request. The bearer token
// the request carries is a credential, not intent, and must never reach disk.
func TestConvergeGraphNeverRecordsTheBearerToken(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	const secret = "tok-SECRET-4f1c"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, secret)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/global/networks"):
			fmt.Fprint(w, `{"name":"op-1","status":"RUNNING"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("c", networks, "insert",
		map[string]string{"project": "demo"}, nil, map[string]any{"name": "net-1", "autoCreateSubnetworks": false})}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	pl, err := omnisdk.Prepare(corpus, omnisdk.GraphStatement(g, omnisdk.NewManaged("net", []string{"c"}, "name", "network")),
		omnisdk.NewLedger("demo", state, "r1"), omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, row := range drain(t, pl) {
		if row["error"] != nil {
			t.Fatalf("run: %v", row)
		}
	}
	if where := secretIn(t, state, secret); len(where) > 0 {
		t.Errorf("bearer token written to %v", where)
	}
}
