package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestCanonicalInputSelection(t *testing.T) {
	for _, test := range []struct {
		name       string
		candidates []omnisdk.InboundCandidate
		supplied   map[string]string
		explicit   bool
		want       []string
	}{
		{"canonical-before-cost", []omnisdk.InboundCandidate{{From: "cheap", Src: "id", As: "parent", Cost: 0}, {From: "canonical", Src: "id", As: "parent", Canonical: true, Cost: 50}}, nil, false, []string{"canonical", "parent-from-canonical"}},
		{"lowest-cost", []omnisdk.InboundCandidate{{From: "expensive", Src: "id", As: "parent", Cost: 50}, {From: "cheap", Src: "id", As: "parent", Cost: 1}}, nil, false, []string{"cheap", "parent-from-cheap"}},
		{"stable-tie", []omnisdk.InboundCandidate{{From: "expensive", Src: "id", As: "parent", Cost: 1}, {From: "cheap", Src: "id", As: "parent", Cost: 1}}, nil, false, []string{"expensive", "parent-from-expensive"}},
		{"caller-value", []omnisdk.InboundCandidate{{From: "canonical", Src: "id", As: "parent", Canonical: true}}, map[string]string{"parent": "caller-parent"}, false, []string{"caller-parent"}},
		{"explicit-edge", []omnisdk.InboundCandidate{{From: "canonical", Src: "id", As: "parent", Canonical: true}}, nil, true, []string{"explicit", "parent-from-explicit"}},
		{"missing-source", []omnisdk.InboundCandidate{{From: "missing", Src: "id", As: "parent", Canonical: true}, {From: "cheap", Src: "id", As: "parent", Cost: 1}}, nil, false, []string{"cheap", "parent-from-cheap"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Every candidate node is explicitly available; unselected possibilities must
			// not execute merely because they were included in the possible-flow graph.
			nodes := []omnisdk.Node{}
			seen := map[string]bool{}
			for _, candidate := range test.candidates {
				if candidate.From == "missing" || seen[candidate.From] {
					continue
				}
				seen[candidate.From] = true
				nodes = append(nodes, omnisdk.NewNode(candidate.From, "stackql_unstable_widgets.things.widgets", map[string]string{"id": candidate.From}))
			}
			inbound := []omnisdk.Inbound{}
			if test.explicit {
				nodes = append(nodes, omnisdk.NewNode("explicit", "stackql_unstable_widgets.things.widgets", map[string]string{"id": "explicit"}))
				inbound = append(inbound, omnisdk.NewInbound("explicit", "id", "parent"))
			}
			nodes = append(nodes, omnisdk.NewNode("consumer", "stackql_unstable_widgets.things.widgets", test.supplied))
			wiring := omnisdk.NewWiringWithTransform("consumer", inbound, dsl.Transform{Type: "starlark", Body: `{"id": inbox["parent"]}`, Required: []string{"parent"}}, "id")
			wiring = omnisdk.WithInboundCandidates(wiring, test.candidates...)
			var projections []omnisdk.Projection
			for _, node := range nodes {
				projection, err := omnisdk.NewProjection(node.Alias(), []omnisdk.SelectColumn{omnisdk.NewSelectColumn("id", omnisdk.NewField("id"))})
				if err != nil {
					t.Fatal(err)
				}
				projections = append(projections, projection)
			}
			graph, err := omnisdk.NewGraphWithFilters(nodes, []omnisdk.Wiring{wiring}, projections, omnisdk.NewRowOps(nil, nil, nil))
			if err != nil {
				t.Fatal(err)
			}
			var requests []string
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id := r.URL.Query().Get("id")
				mu.Lock()
				requests = append(requests, id)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `[{"id":%q,"size":"S","color":"blue"}]`, "parent-from-"+id)
			}))
			defer srv.Close()
			pl, err := omnisdk.NewGraphQuery(authRegistry, graph, omnisdk.Args{Endpoint: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := pl.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(requests, test.want) {
				t.Fatalf("requests=%v want=%v", requests, test.want)
			}
		})
	}
}

func TestCanonicalRequiredInputIsCheckedBeforeEffects(t *testing.T) {
	node := omnisdk.NewMutationNode("consumer", "stackql_unstable_widgets.things.widgets", "insert", nil, nil, map[string]any{"size": "S"})
	wiring := omnisdk.NewWiringWithTransform("consumer", nil, dsl.Transform{Type: "starlark", Body: `{"size":"S"}`, Required: []string{"parent"}}, "size")
	graph, err := omnisdk.NewGraph([]omnisdk.Node{node}, []omnisdk.Wiring{wiring})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := omnisdk.NewGraphQuery(authRegistry, graph, omnisdk.Args{}); err == nil {
		t.Fatal("accepted an unsatisfied hard-required input")
	}
}

func TestCanonicalPrunesUnselectedCatalogLookups(t *testing.T) {
	nodes := []omnisdk.Node{omnisdk.NewNode("unused", "missing.provider.resource", nil), omnisdk.NewNode("consumer", "stackql_unstable_widgets.things.widgets", map[string]string{"id": "given"})}
	wiring := omnisdk.WithInboundCandidates(omnisdk.NewWiringWithTransform("consumer", nil, dsl.Transform{Required: []string{"id"}}), omnisdk.InboundCandidate{From: "unused", Src: "id", Canonical: true})
	graph, err := omnisdk.NewGraph(nodes, []omnisdk.Wiring{wiring})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := omnisdk.NewGraphQuery(authRegistry, graph, omnisdk.Args{}); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalCallerInputCanBeTransformedUnderSameName(t *testing.T) {
	node := omnisdk.NewNode("consumer", "stackql_unstable_widgets.things.widgets", map[string]string{"id": "given"})
	w := omnisdk.NewWiringWithTransform("consumer", nil, dsl.Transform{Type: "starlark", Body: `{"id": inbox["id"]}`, Required: []string{"id"}}, "id")
	graph, err := omnisdk.NewGraph([]omnisdk.Node{node}, []omnisdk.Wiring{w})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := omnisdk.NewGraphQuery(authRegistry, graph, omnisdk.Args{}); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalRequiredInputCannotBeWaivedByAdmission(t *testing.T) {
	for _, test := range []struct {
		name, value string
		wantCalls   int
		wantError   bool
	}{
		{"null", "null", 1, true}, {"false", "false", 2, false}, {"zero", "0", 2, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := omnisdk.NewNode("source", "stackql_unstable_widgets.things.widgets", map[string]string{"id": "source"})
			consumer := omnisdk.NewNode("consumer", "stackql_unstable_widgets.things.widgets", nil)
			wiring := omnisdk.WithInboundCandidates(omnisdk.NewWiringWithTransform("consumer", nil, dsl.Transform{
				Type: "starlark", Body: `{"id":"fixed"}`, Required: []string{"parent"}, Admission: &dsl.Program{Type: "starlark", Body: `True`},
			}, "id"), omnisdk.InboundCandidate{From: "source", Src: "id", As: "parent", Canonical: true})
			graph, err := omnisdk.NewGraph([]omnisdk.Node{source, consumer}, []omnisdk.Wiring{wiring})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `[{"id":%s}]`, test.value)
			}))
			defer srv.Close()
			pl, err := omnisdk.NewGraphQuery(authRegistry, graph, omnisdk.Args{Endpoint: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := pl.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
			}
			if (rows.Err() != nil) != test.wantError || calls != test.wantCalls {
				t.Fatalf("calls=%d err=%v", calls, rows.Err())
			}
		})
	}
}
