package omnisdk_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

const networks = "stackql_unstable_google.compute.networks"

// networksHaveARowPath gives compute.networks.list the row path its document omits.
var networksHaveARowPath = omnisdk.DocPatch{
	Service: "stackql_unstable_google.compute",
	Merge: json.RawMessage(`{"components":{"x-stackQL-resources":{"networks":{"methods":{"list":` +
		`{"response":{"objectKey":"$.items"}}}}}}}`),
}

// countNetworks lists networks from dir against a stub answering two, and counts the rows.
func countNetworks(t *testing.T, dir string) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
			return
		}
		fmt.Fprint(w, `{"kind":"compute#networkList","items":[{"name":"n1"},{"name":"n2"}]}`)
	}))
	defer srv.Close()
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("n", networks, nil)}, nil)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(dir, g, omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"project": "demo"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return n
}

func fileSum(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// A patched view changes what its caller reads and nothing anyone else reads.
func TestPatchedViewIsTheCallersAlone(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	baseDoc := filepath.Join(corpus, "googleapis.com", "v00.00.00000", "services", "compute.yaml")
	before := fileSum(t, baseDoc)

	view, err := omnisdk.EffectiveRegistry(corpus, []omnisdk.DocPatch{networksHaveARowPath}, omnisdk.DocCache{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if got := countNetworks(t, view); got != 2 {
		t.Errorf("patched view: %d rows, want one per network", got)
	}
	if got := countNetworks(t, corpus); got != 1 {
		t.Errorf("base: %d rows, want the unpatched document's single envelope", got)
	}
	if fileSum(t, baseDoc) != before {
		t.Error("the base document changed")
	}
}

// The same view is built once and reused; Fresh rebuilds it; a new cache is a new location.
func TestDocCacheReuseFreshAndNew(t *testing.T) {
	requireCorpus(t)
	parent := t.TempDir()
	cacheDir, err := omnisdk.NewDocCache(parent)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	patches := []omnisdk.DocPatch{networksHaveARowPath}
	first, err := omnisdk.EffectiveRegistry(corpus, patches, omnisdk.DocCache{Dir: cacheDir})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	built := modTime(t, filepath.Join(first, ".complete"))

	time.Sleep(10 * time.Millisecond)
	again, err := omnisdk.EffectiveRegistry(corpus, patches, omnisdk.DocCache{Dir: cacheDir})
	if err != nil {
		t.Fatalf("again: %v", err)
	}
	if again != first || !modTime(t, filepath.Join(again, ".complete")).Equal(built) {
		t.Errorf("reuse rebuilt the view")
	}

	fresh, err := omnisdk.EffectiveRegistry(corpus, patches, omnisdk.DocCache{Dir: cacheDir, Fresh: true})
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if fresh != first || !modTime(t, filepath.Join(fresh, ".complete")).After(built) {
		t.Errorf("fresh did not rebuild the view")
	}

	other, err := omnisdk.NewDocCache(parent)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	if other == cacheDir {
		t.Error("a new cache reused an old location")
	}
}

func modTime(t *testing.T, p string) time.Time {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func TestEffectiveRegistryRefusesWhatItCannotPlace(t *testing.T) {
	requireCorpus(t)
	cases := map[string]struct {
		patches []omnisdk.DocPatch
		cache   omnisdk.DocCache
	}{
		"no cache directory": {patches: []omnisdk.DocPatch{networksHaveARowPath}},
		"unknown service": {
			patches: []omnisdk.DocPatch{{Service: "stackql_unstable_google.nope", Merge: json.RawMessage(`{}`)}},
			cache:   omnisdk.DocCache{Dir: t.TempDir()},
		},
		"patch not JSON": {
			patches: []omnisdk.DocPatch{{Service: "stackql_unstable_google.compute", Merge: json.RawMessage(`{`)}},
			cache:   omnisdk.DocCache{Dir: t.TempDir()},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := omnisdk.EffectiveRegistry(corpus, tc.patches, tc.cache); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// globalOperationsHaveAGet gives global_operations the get its document declares a path for but no
// method: the view a caller needs to follow a create's operation.
var globalOperationsHaveAGet = omnisdk.DocPatch{
	Service: "stackql_unstable_google.compute",
	Merge: json.RawMessage(`{"components":{"x-stackQL-resources":{"global_operations":{
		"methods":{"get":{
			"operation":{"$ref":"#/paths/~1projects~1{project}~1global~1operations~1{operation}/get"},
			"response":{"mediaType":"application/json","openAPIDocKey":"200"}}},
		"sqlVerbs":{"select":[
			{"$ref":"#/components/x-stackQL-resources/global_operations/methods/aggregated_list"},
			{"$ref":"#/components/x-stackQL-resources/global_operations/methods/get"}]}}}}}`),
}

// Create a network, poll its operation until DONE, then read what it made — one graph, every
// node's columns in the row. The operation get exists only in the caller's patched view.
func TestCreatePollThenGet(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	var opPolls int
	var created string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/token"):
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/global/networks"):
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			created = string(b)
			mu.Unlock()
			fmt.Fprint(w, `{"name":"op-1","status":"RUNNING"}`)
		case strings.HasSuffix(p, "/global/operations/op-1"):
			mu.Lock()
			opPolls++
			n := opPolls
			mu.Unlock()
			status := "RUNNING"
			if n >= 3 {
				status = "DONE"
			}
			fmt.Fprintf(w, `{"name":"op-1","status":%q,"targetLink":"projects/demo/global/networks/net-1"}`, status)
		case strings.HasSuffix(p, "/global/networks/net-1"):
			fmt.Fprint(w, `{"name":"net-1","id":"123","autoCreateSubnetworks":false}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+p, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	view, err := omnisdk.EffectiveRegistry(corpus, []omnisdk.DocPatch{globalOperationsHaveAGet}, omnisdk.DocCache{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	const operations = "stackql_unstable_google.compute.global_operations"
	poll, err := omnisdk.NewPollOverride(operations, omnisdk.Poll{
		StatusPath: "status", Done: "DONE", Interval: 10 * time.Millisecond, MaxAttempts: 10,
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	project := map[string]string{"project": "demo"}
	g, err := omnisdk.NewGraph(
		[]omnisdk.Node{
			omnisdk.NewMutationNode("c", networks, "insert", project, nil,
				map[string]any{"name": "net-1", "autoCreateSubnetworks": false}),
			omnisdk.NewNode("o", operations, project),
			omnisdk.NewNode("n", networks, project),
		},
		[]omnisdk.Wiring{
			omnisdk.NewWiring("o", []omnisdk.Inbound{omnisdk.NewInbound("c", "name", "operation")}, "", ""),
			omnisdk.NewWiring("n", []omnisdk.Inbound{omnisdk.NewInbound("o", "targetLink", "link")},
				"golang_template_json_v0.1.0", `{"network":"{{ index (split "/" .link) 4 }}"}`, "network"),
		},
		poll,
	)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	got := runCreatePoll(t, view, g, omnisdk.Args{Endpoint: srv.URL})
	if opPolls != 3 {
		t.Errorf("operation polled %d times, want until DONE on the third", opPolls)
	}
	// A body constant keeps its JSON type: false, not "false".
	var sent map[string]any
	if err := json.Unmarshal([]byte(created), &sent); err != nil || sent["name"] != "net-1" || sent["autoCreateSubnetworks"] != false {
		t.Errorf("create body = %s, want name and a boolean autoCreateSubnetworks", created)
	}
	if len(got) != 1 || got[0]["id"] != "123" || got[0]["status"] != "DONE" {
		t.Fatalf("rows = %v, want the created network's detail beside its finished operation", got)
	}
	// Service-account auth carries a signed assertion and a bearer token on the row; by default
	// neither leaves in the result.
	for _, k := range []string{"assertion", "token"} {
		if _, leaked := got[0][k]; leaked {
			t.Errorf("row carries credential %q", k)
		}
	}

	// Redaction is the caller's policy: a power user keeps credentials, or drops more.
	opPolls = 0
	kept := runCreatePoll(t, view, g, omnisdk.Args{Endpoint: srv.URL, Redaction: omnisdk.RedactNone()})
	if kept[0]["token"] != "tok" {
		t.Errorf("RedactNone dropped the token: %v", kept[0])
	}
	opPolls = 0
	less := runCreatePoll(t, view, g, omnisdk.Args{Endpoint: srv.URL,
		Redaction: omnisdk.RedactAlso(omnisdk.DefaultRedaction(), "targetLink")})
	if _, ok := less[0]["targetLink"]; ok {
		t.Errorf("RedactAlso kept targetLink: %v", less[0])
	}
	if _, ok := less[0]["token"]; ok {
		t.Errorf("RedactAlso lost the default: %v", less[0])
	}
}

func runCreatePoll(t *testing.T, view string, g omnisdk.Graph, args omnisdk.Args) []omnisdk.Row {
	t.Helper()
	pl, err := omnisdk.NewGraphSelectQuery(view, g, args)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rows.Close()
	var got []omnisdk.Row
	for rows.Next() {
		got = append(got, rows.Row())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return got
}
