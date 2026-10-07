package omnisdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
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

// UNION ALL: every leg's rows in one stream, and one Limit across them.
func TestUnionAll(t *testing.T) {
	var calls atomic.Int64
	srv := treeServer(t, map[string][]string{"x": {"a", "b"}, "y": {"c"}}, &calls)
	defer srv.Close()
	leg := func(parent string, limit int) omnisdk.Plan {
		g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("f", folders, map[string]string{"parent": parent})}, nil)
		if err != nil {
			t.Fatal(err)
		}
		pl, err := omnisdk.NewGraphSelectQuery(authRegistry, g, omnisdk.Args{Endpoint: srv.URL, Tuning: omnisdk.Tuning{Limit: limit}})
		if err != nil {
			t.Fatal(err)
		}
		return pl
	}
	run := func(pl omnisdk.Plan) []string {
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
			t.Fatal(err)
		}
		slices.Sort(ids)
		return ids
	}
	all, err := omnisdk.UnionAll(leg("x", 0), leg("y", 0))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(run(all), ","); got != "a,b,c" {
		t.Errorf("union = %s, want a,b,c", got)
	}
	capped, _ := omnisdk.UnionAll(leg("x", 2), leg("y", 0))
	if got := run(capped); len(got) != 2 {
		t.Errorf("union under limit 2 = %v", got)
	}
}

// A left join needs only its ON: the other node's column reaches the joined node without a wiring.
func TestLeftJoinByOnAlone(t *testing.T) {
	var calls atomic.Int64
	srv := treeServer(t, map[string][]string{"x": {"a", "b"}, "y": {"b"}}, &calls)
	defer srv.Close()
	g, err := omnisdk.NewGraphWithProjections(
		[]omnisdk.Node{
			omnisdk.NewNode("l", folders, map[string]string{"parent": "x"}),
			omnisdk.NewOuterNode(omnisdk.NewNode("r", folders, map[string]string{"parent": "y"}),
				[]query.Predicate{query.NewEq(query.NewColumn("r", "rid"), query.NewColumn("l", "lid"))}),
		}, nil,
		[]omnisdk.Projection{
			mustProjection(t, "l", omnisdk.NewSelectColumn("lid", omnisdk.NewField("id"))),
			mustProjection(t, "r", omnisdk.NewSelectColumn("rid", omnisdk.NewField("id"))),
		})
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(authRegistry, g, omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		r := rows.Row()
		got = append(got, fmt.Sprintf("%v/%v", r["lid"], r["rid"]))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if strings.Join(got, ",") != "a/<nil>,b/b" {
		t.Errorf("rows = %v, want a unmatched and b matched", got)
	}
}

func mustProjection(t *testing.T, alias string, cols ...omnisdk.SelectColumn) omnisdk.Projection {
	t.Helper()
	p, err := omnisdk.NewProjection(alias, cols)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
