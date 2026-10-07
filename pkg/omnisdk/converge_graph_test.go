package omnisdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

const widgets = "stackql_unstable_widgets.things.widgets"

// widgetStore is a stand-in API over widgets, recording each call. A widget whose size is "bad" is
// refused at create.
type widgetStore struct {
	mu    sync.Mutex
	items map[string]map[string]any
	calls []string
	n     int
}

func (s *widgetStore) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		id := strings.TrimPrefix(r.URL.Path, "/widgets/")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Method == http.MethodGet:
			s.calls = append(s.calls, "find")
			out := []map[string]any{}
			if it, ok := s.items[r.URL.Query().Get("id")]; ok {
				out = append(out, it)
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost:
			s.calls = append(s.calls, "create")
			if body["size"] == "bad" {
				http.Error(w, `{"error":"bad size"}`, http.StatusBadRequest)
				return
			}
			s.n++
			body["id"] = fmt.Sprintf("w-%d", s.n)
			s.items[body["id"].(string)] = body
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPatch:
			s.calls = append(s.calls, "update")
			for k, v := range body {
				s.items[id][k] = v
			}
			_ = json.NewEncoder(w).Encode(s.items[id])
		case r.Method == http.MethodDelete:
			s.calls = append(s.calls, "delete "+id)
			delete(s.items, id)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
}

func (s *widgetStore) took() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.calls
	s.calls = nil
	return out
}

// widgetResource is the subgraph converging one widget: recall it, read it, diff it, and branch to a
// create when absent or an update when drifted.
func widgetResource(key string, desired map[string]any) ([]omnisdk.Node, []omnisdk.Wiring, omnisdk.Branch, []omnisdk.Gate, omnisdk.Managed) {
	r, live, d := key+"_r", key+"_live", key+"_d"
	create, update := key+"_create", key+"_update"
	nodes := []omnisdk.Node{
		omnisdk.NewRecallNode(r, key),
		omnisdk.NewOuterNode(omnisdk.NewNode(live, widgets, nil), nil),
		omnisdk.NewDiffNode(d, key, live, "id", desired),
		omnisdk.NewMutationNode(create, widgets, "insert", nil, nil, map[string]any{"size": nil, "color": nil}),
		omnisdk.NewMutationNode(update, widgets, "update", nil, nil, map[string]any{"size": nil, "color": nil}),
	}
	wirings := []omnisdk.Wiring{
		omnisdk.NewWiring(live, []omnisdk.Inbound{omnisdk.NewInbound(r, "identity", "id")}, "", ""),
		omnisdk.NewWiring(create, []omnisdk.Inbound{omnisdk.NewInbound(d, "size", "size"), omnisdk.NewInbound(d, "color", "color")}, "", ""),
		omnisdk.NewWiring(update, []omnisdk.Inbound{omnisdk.NewInbound(d, "size", "size"), omnisdk.NewInbound(d, "color", "color"),
			omnisdk.NewInbound(d, "identity", "id")}, "", ""),
	}
	status := query.NewColumn(d, "status")
	br, _ := omnisdk.NewBranch(key+"_b",
		omnisdk.NewArm("create", query.NewEq(status, query.NewLiteral(omnisdk.DiffAbsent))),
		omnisdk.NewArm("update", query.NewEq(status, query.NewLiteral(omnisdk.DiffDrift))),
		omnisdk.Otherwise("keep"))
	gates := []omnisdk.Gate{omnisdk.NewGate(key+"_b", "create", create), omnisdk.NewGate(key+"_b", "update", update)}
	return nodes, wirings, br, gates, omnisdk.NewManaged(key, []string{create, update}, "id", "id")
}

func convergeWidgets(t *testing.T, srv *httptest.Server, state string, desired map[string]map[string]any, order []string) []omnisdk.Row {
	t.Helper()
	var nodes []omnisdk.Node
	var wirings []omnisdk.Wiring
	var managed []omnisdk.Managed
	type bg struct {
		b omnisdk.Branch
		g []omnisdk.Gate
	}
	var branches []bg
	for _, k := range order {
		n, w, b, g, m := widgetResource(k, desired[k])
		nodes, wirings, managed = append(nodes, n...), append(wirings, w...), append(managed, m)
		branches = append(branches, bg{b, g})
	}
	g, err := omnisdk.NewGraph(nodes, wirings)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range branches {
		if g, err = omnisdk.WithBranch(g, x.b, x.g...); err != nil {
			t.Fatal(err)
		}
	}
	pl, err := omnisdk.ConvergeGraph(authRegistry, "demo", state, "", g, managed, omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rows.Close()
	var out []omnisdk.Row
	for rows.Next() {
		out = append(out, rows.Row())
	}
	return out
}

// Absent → create; unchanged → nothing; changed → update. One graph, branching per run on what the
// diff finds.
func TestConvergeGraphCreatesKeepsAndUpdates(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{}}
	srv := store.serve(t)
	defer srv.Close()
	state := t.TempDir()
	want := map[string]map[string]any{"w1": {"size": "L", "color": "red"}}

	convergeWidgets(t, srv, state, want, []string{"w1"})
	if got := strings.Join(store.took(), ","); got != "create" {
		t.Errorf("first run: calls %s, want a create (nothing to find yet)", got)
	}
	convergeWidgets(t, srv, state, want, []string{"w1"})
	if got := strings.Join(store.took(), ","); got != "find" {
		t.Errorf("second run: calls %s, want only a read", got)
	}
	want["w1"]["color"] = "blue"
	convergeWidgets(t, srv, state, want, []string{"w1"})
	if got := strings.Join(store.took(), ","); got != "find,update" {
		t.Errorf("third run: calls %s, want a read and an update", got)
	}
	if store.items["w-1"]["color"] != "blue" || len(store.items) != 1 {
		t.Errorf("store = %v, want one blue widget", store.items)
	}
}

// A failed run compensates what it created: w1 lands, w2 is refused, and w1 is deleted.
func TestConvergeGraphCompensatesAFailedRun(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{}}
	srv := store.serve(t)
	defer srv.Close()
	rows := convergeWidgets(t, srv, t.TempDir(), map[string]map[string]any{
		"w1": {"size": "L", "color": "red"}, "w2": {"size": "bad", "color": "red"},
	}, []string{"w1", "w2"})
	if len(store.items) != 0 {
		t.Errorf("store = %v, want w1 compensated", store.items)
	}
	last := rows[len(rows)-1]
	if last["status"] != "failed" || fmt.Sprint(last["compensated"]) != "[demo/w1]" {
		t.Errorf("last row = %v, want failed with w1 compensated", last)
	}
}
