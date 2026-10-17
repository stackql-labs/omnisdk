package omnisdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

const subnetworks = "stackql_unstable_google.compute.subnetworks"

// networksAwaitTheirOperation is the session addendum: a create of a network answers with an
// operation, and is finished only when that operation's get reports DONE. It also gives
// global_operations the get the base document declares a path for but no method.
var networksAwaitTheirOperation = omnisdk.DocPatch{
	Service: "stackql_unstable_google.compute",
	Merge: json.RawMessage(`{"components":{"x-stackQL-resources":{
		"global_operations":{"methods":{"get":{
			"operation":{"$ref":"#/paths/~1projects~1{project}~1global~1operations~1{operation}/get"},
			"response":{"mediaType":"application/json","openAPIDocKey":"200"}}}},
		"networks":{"methods":{"insert":{"x-stackQL-beta":{
			"method":{"$ref":"#/components/x-stackQL-resources/global_operations/methods/get"},
			"bind":{"operation":"name"},
			"await":{"statusPath":"status","done":"DONE","errorPath":"error","interval":"5ms","maxAttempts":5}}}}}}}}`),
}

// gcpNetworks fakes the network and subnetwork API. The network's operation reports DONE on the
// pollsUntilDone-th get, or never where that is zero; finalErr is the error a DONE operation carries.
type gcpNetworks struct {
	mu             sync.Mutex
	events         []string
	polls          int
	pollsUntilDone int
	finalErr       string
	subnetBody     map[string]any
}

func (g *gcpNetworks) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/token"):
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/global/networks"):
			g.events = append(g.events, "create network")
			fmt.Fprint(w, `{"name":"op-1","status":"RUNNING"}`)
		case strings.HasSuffix(p, "/global/operations/op-1"):
			g.polls++
			if g.pollsUntilDone == 0 || g.polls < g.pollsUntilDone {
				fmt.Fprint(w, `{"name":"op-1","status":"RUNNING"}`)
				return
			}
			g.events = append(g.events, "network done")
			errField := ""
			if g.finalErr != "" {
				errField = fmt.Sprintf(`,"error":{"errors":[{"code":%q}]}`, g.finalErr)
			}
			fmt.Fprintf(w, `{"name":"op-1","status":"DONE","targetLink":"projects/demo/global/networks/net-1"%s}`, errField)
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/subnetworks"):
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &g.subnetBody)
			g.events = append(g.events, "create subnetwork")
			fmt.Fprint(w, `{"name":"op-2","status":"RUNNING"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+p, http.StatusNotFound)
		}
	}))
}

// networkThenSubnet creates a network and, over a β edge from what the create answers, a subnetwork
// in it. The edge carries the network's targetLink, which only a finished operation reports.
func networkThenSubnet(t *testing.T, view string, srv *httptest.Server) ([]omnisdk.Row, error) {
	t.Helper()
	g, err := omnisdk.NewGraph(
		[]omnisdk.Node{
			omnisdk.NewMutationNode("net", networks, "insert", map[string]string{"project": "demo"}, nil,
				map[string]any{"name": "net-1", "autoCreateSubnetworks": false}),
			omnisdk.NewMutationNode("sub", subnetworks, "insert", map[string]string{"project": "demo", "region": "us-east1"}, nil,
				map[string]any{"name": "sub-1", "ipCidrRange": "10.0.0.0/24", "network": nil}),
		},
		[]omnisdk.Wiring{
			omnisdk.NewWiring("sub", []omnisdk.Inbound{omnisdk.NewInbound("net", "targetLink", "network")}, "", ""),
		},
	)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	pl, err := omnisdk.Prepare(view, omnisdk.GraphStatement(g), omnisdk.NoWAL(), omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []omnisdk.Row
	for rows.Next() {
		out = append(out, rows.Row())
	}
	return out, rows.Err()
}

// The patched document says a network create is finished when its operation is DONE, so the
// subnetwork depending on it is created only then, from the link the finished operation reports.
// Nothing in the graph says to wait: the document does.
func TestBetaAwaitsTheCreateBeforeItsDependent(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	view, err := omnisdk.EffectiveRegistry(corpus, []omnisdk.DocPatch{networksAwaitTheirOperation}, omnisdk.DocCache{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	api := &gcpNetworks{pollsUntilDone: 3}
	srv := api.serve(t)
	defer srv.Close()

	if _, err := networkThenSubnet(t, view, srv); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.Join(api.events, ", "); got != "create network, network done, create subnetwork" {
		t.Errorf("events = %s, want the subnetwork created only after the network is done", got)
	}
	if api.polls != 3 {
		t.Errorf("operation polled %d times, want until DONE on the third", api.polls)
	}
	if api.subnetBody["network"] != "projects/demo/global/networks/net-1" {
		t.Errorf("subnetwork body = %v, want the finished network's link", api.subnetBody)
	}
}

// An operation that never reports done may or may not have made the network, and is reported so; one
// that reports done with an error failed. Either way nothing depending on it is created.
func TestBetaUnfinishedOrFailedStopsTheDependent(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	view, err := omnisdk.EffectiveRegistry(corpus, []omnisdk.DocPatch{networksAwaitTheirOperation}, omnisdk.DocCache{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	for name, api := range map[string]*gcpNetworks{
		"never done":      {pollsUntilDone: 0},
		"done with error": {pollsUntilDone: 1, finalErr: "QUOTA_EXCEEDED"},
	} {
		srv := api.serve(t)
		_, err := networkThenSubnet(t, view, srv)
		srv.Close()
		if err == nil {
			t.Errorf("%s: run succeeded; want an error", name)
			continue
		}
		if !errors.Is(err, omnisdk.ErrOutcomeUnknown) {
			t.Errorf("%s: err = %v; want it reported as possibly applied, never as rejected", name, err)
		}
		if name == "done with error" && !strings.Contains(err.Error(), "QUOTA_EXCEEDED") {
			t.Errorf("%s: err = %v; want the operation's error", name, err)
		}
		for _, e := range api.events {
			if e == "create subnetwork" {
				t.Errorf("%s: the subnetwork was created", name)
			}
		}
	}
}

// A beta that cannot be followed is refused when the plan is built, never dropped: without it the
// create would read as finished the moment it answers.
func TestBetaMalformedIsRefused(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	for name, beta := range map[string]string{
		"no attempts":   `{"method":{"$ref":"#/components/x-stackQL-resources/networks/methods/get"},"await":{"statusPath":"status","done":"DONE","interval":"1s"}}`,
		"no interval":   `{"method":{"$ref":"#/components/x-stackQL-resources/networks/methods/get"},"await":{"statusPath":"status","done":"DONE","maxAttempts":3}}`,
		"no status":     `{"method":{"$ref":"#/components/x-stackQL-resources/networks/methods/get"},"await":{"interval":"1s","maxAttempts":3}}`,
		"bad method":    `{"method":{"$ref":"#/paths/nowhere"},"await":{"statusPath":"status","done":"DONE","interval":"1s","maxAttempts":3}}`,
		"absent method": `{"method":{"$ref":"#/components/x-stackQL-resources/networks/methods/nope"},"await":{"statusPath":"status","done":"DONE","interval":"1s","maxAttempts":3}}`,
	} {
		patch := omnisdk.DocPatch{Service: "stackql_unstable_google.compute",
			Merge: json.RawMessage(`{"components":{"x-stackQL-resources":{"networks":{"methods":{"insert":{"x-stackQL-beta":` + beta + `}}}}}}`)}
		view, err := omnisdk.EffectiveRegistry(corpus, []omnisdk.DocPatch{patch}, omnisdk.DocCache{Dir: t.TempDir()})
		if err != nil {
			t.Fatalf("%s: view: %v", name, err)
		}
		g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("net", networks, "insert",
			map[string]string{"project": "demo"}, nil, map[string]any{"name": "net-1"})}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := omnisdk.Prepare(view, omnisdk.GraphStatement(g), omnisdk.NoWAL(), omnisdk.Args{Endpoint: "http://127.0.0.1:1"}); err == nil ||
			!strings.Contains(err.Error(), "x-stackQL-beta") {
			t.Errorf("%s: err = %v, want a refusal naming x-stackQL-beta", name, err)
		}
	}
}
