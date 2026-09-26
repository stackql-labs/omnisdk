package omnisdk_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

const authRegistry = "testdata/authreg"

// One query reads three providers, each authenticated its own way with its own credentials: basic
// from the environment variables its document names, a bearer token and a header key from
// AuthByProvider — which wins over the query-wide Auth.
func TestEachProviderAuthenticatesItsOwnWay(t *testing.T) {
	t.Setenv("TEST_BASIC_USER", "alice")
	t.Setenv("TEST_BASIC_PASS", "s3cret")
	var mu sync.Mutex
	seen := map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[strings.Split(strings.Trim(r.URL.Path, "/"), "/")[0]] = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"id":"1"}]}`)
	}))
	defer srv.Close()

	g, err := omnisdk.NewGraph([]omnisdk.Node{
		omnisdk.NewNode("b", "stackql_unstable_basicp.things.items", nil),
		omnisdk.NewNode("t", "stackql_unstable_bearp.things.items", nil),
		omnisdk.NewNode("k", "stackql_unstable_keyed.things.items", nil),
	}, nil)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(authRegistry, g, omnisdk.Args{
		Endpoint: srv.URL,
		Auth:     &omnisdk.Auth{Credentials: "query-wide"},
		AuthByProvider: map[string]*omnisdk.Auth{
			"stackql_unstable_bearp": {Credentials: "tok-b"},
			"keyed":                  {Credentials: "key-k"},
			"basicp":                 {},
		},
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
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	if got := seen["basicp"].Get("Authorization"); got != basic {
		t.Errorf("basic: Authorization = %q, want %q", got, basic)
	}
	if got := seen["bearp"].Get("Authorization"); got != "Bearer tok-b" {
		t.Errorf("bearer: Authorization = %q", got)
	}
	if got := seen["keyed"].Get("x-api-key"); got != "key-k" {
		t.Errorf("custom: x-api-key = %q", got)
	}
}

// A provider whose document requires auth, with no credential anywhere, is refused before any request.
func TestMissingCredentialIsRefused(t *testing.T) {
	t.Setenv("TEST_BEARER_TOKEN", "")
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("t", "stackql_unstable_bearp.things.items", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = omnisdk.NewGraphSelectQuery(authRegistry, g, omnisdk.Args{Endpoint: "http://127.0.0.1:9"})
	if err == nil || !strings.Contains(err.Error(), "bearp bearer credentials cannot be used") {
		t.Errorf("err = %v, want the missing bearer credential named", err)
	}
}

// Columns carry the type their document declares.
func TestColumnTypesComeFromTheSchema(t *testing.T) {
	requireCorpus(t)
	tbl, err := omnisdk.DescribeTable(corpus, "stackql_unstable_google.storage.buckets")
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]omnisdk.ColumnType{}
	for _, m := range tbl.Methods() {
		for k, v := range m.ColumnTypes() {
			types[k] = v
		}
	}
	if got := types["name"]; got.Type != "string" {
		t.Errorf("name = %+v, want string", got)
	}
	if got := types["timeCreated"]; got.Type != "string" || got.Format != "date-time" {
		t.Errorf("timeCreated = %+v, want string/date-time", got)
	}
	if got := types["metageneration"]; got.Type == "" {
		t.Errorf("metageneration has no type: %+v", got)
	}
}
