package omnisdk_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// treeServer answers the children of a folder.
func treeServer(t *testing.T, children map[string][]string, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var out []map[string]string
		for _, c := range children[r.URL.Query().Get("parent")] {
			out = append(out, map[string]string{"id": c})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
}

const folders = "stackql_unstable_tree.things.folders"

// crawlGraph is one node listing a folder's children, wired to itself: each child it finds is the
// parent of its next request. The first parent is a constant.
func crawlGraph(t *testing.T) omnisdk.Graph {
	t.Helper()
	g, err := omnisdk.NewGraph(
		[]omnisdk.Node{omnisdk.NewNode("f", folders, map[string]string{"parent": "root"})},
		[]omnisdk.Wiring{omnisdk.NewWiring("f", []omnisdk.Inbound{omnisdk.NewInbound("f", "id", "parent")}, "", "")},
	)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func crawlIDs(t *testing.T, g omnisdk.Graph, srv *httptest.Server) []string {
	t.Helper()
	pl, err := omnisdk.NewGraphSelectQuery(authRegistry, g, omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		ids = append(ids, rows.Row()["id"].(string))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	slices.Sort(ids)
	return ids
}

// A node wired to itself crawls the tree to its leaves, then stops: a round finds nothing new.
func TestSelfWiredNodeCrawls(t *testing.T) {
	var calls atomic.Int64
	srv := treeServer(t, map[string][]string{"root": {"a", "b"}, "a": {"c"}}, &calls)
	defer srv.Close()
	g, err := omnisdk.WithTermination(crawlGraph(t), "f", omnisdk.Rounds(10))
	if err != nil {
		t.Fatal(err)
	}
	if got := crawlIDs(t, g, srv); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("ids = %v, want a,b,c", got)
	}
}

// A tree deeper than the bound is cut off at it.
func TestCrawlStopsAtItsBound(t *testing.T) {
	var calls atomic.Int64
	chain := map[string][]string{"root": {"1"}, "1": {"2"}, "2": {"3"}, "3": {"4"}, "4": {"5"}}
	srv := treeServer(t, chain, &calls)
	defer srv.Close()
	g, err := omnisdk.WithTermination(crawlGraph(t), "f", omnisdk.Rounds(2))
	if err != nil {
		t.Fatal(err)
	}
	if got := crawlIDs(t, g, srv); strings.Join(got, ",") != "1,2,3" {
		t.Errorf("ids = %v, want the entry's run and two rounds", got)
	}
}

// A cycle with no termination, or one that is not well-founded, is refused when the query is
// planned; nothing is sent.
func TestCycleWithoutAWellFoundedTerminationIsRefused(t *testing.T) {
	var calls atomic.Int64
	srv := treeServer(t, map[string][]string{"root": {"a"}}, &calls)
	defer srv.Close()
	cases := map[string]omnisdk.Graph{"none": crawlGraph(t)}
	for name, term := range map[string]omnisdk.Termination{
		"zero rounds":   omnisdk.Rounds(0),
		"all unbounded": omnisdk.AllOf(omnisdk.Rounds(3), omnisdk.Records(0)),
	} {
		g, err := omnisdk.WithTermination(crawlGraph(t), "f", term)
		if err != nil {
			t.Fatal(err)
		}
		cases[name] = g
	}
	for name, g := range cases {
		_, err := omnisdk.NewGraphSelectQuery(authRegistry, g, omnisdk.Args{Endpoint: srv.URL})
		if err == nil || !strings.Contains(err.Error(), "cycle through") {
			t.Errorf("%s: err = %v, want the cycle refused", name, err)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d requests sent for refused plans", calls.Load())
	}
}
