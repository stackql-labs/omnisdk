package omnisdk_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

// A branch decides per row which nodes run: only folder "a" has its children listed; folder "b"
// passes through without a request.
func TestBranchGatesANode(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	children := map[string][]string{"root": {"a", "b"}, "a": {"c"}, "b": {"d"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("parent")
		mu.Lock()
		asked = append(asked, p)
		mu.Unlock()
		var out []map[string]string
		for _, c := range children[p] {
			out = append(out, map[string]string{"id": c})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	g, err := omnisdk.NewGraph(
		[]omnisdk.Node{
			omnisdk.NewNode("top", folders, map[string]string{"parent": "root"}),
			omnisdk.NewNode("sub", folders, nil),
		},
		[]omnisdk.Wiring{omnisdk.NewWiring("sub", []omnisdk.Inbound{omnisdk.NewInbound("top", "id", "parent")}, "", "")},
	)
	if err != nil {
		t.Fatal(err)
	}
	br, err := omnisdk.NewBranch("pick",
		omnisdk.NewArm("deep", query.NewEq(query.NewColumn("top", "id"), query.NewLiteral("a"))),
		omnisdk.Otherwise("shallow"))
	if err != nil {
		t.Fatal(err)
	}
	if g, err = omnisdk.WithBranch(g, br, omnisdk.NewGate("pick", "deep", "sub")); err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(authRegistry, g, omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	slices.Sort(asked)
	if got := strings.Join(asked, ","); got != "a,root" {
		t.Errorf("requests for parents %s, want root then a only", got)
	}
	if n != 2 {
		t.Errorf("rows = %d, want a with its child and b passed through", n)
	}
}

// A branch is refused where it cannot mean anything: a catch-all before other arms, two arms with
// one label, or a gate to an arm or node that does not exist.
func TestBranchIsChecked(t *testing.T) {
	if _, err := omnisdk.NewBranch("b", omnisdk.Otherwise("x"), omnisdk.NewArm("y", query.NewEq(query.NewColumn("n", "c"), query.NewLiteral(1)))); err == nil {
		t.Error("a catch-all before another arm was accepted")
	}
	if _, err := omnisdk.NewBranch("b", omnisdk.Otherwise("x"), omnisdk.Otherwise("x")); err == nil {
		t.Error("two arms with one label were accepted")
	}
	g, _ := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("n", folders, nil)}, nil)
	br, _ := omnisdk.NewBranch("b", omnisdk.Otherwise("all"))
	if _, err := omnisdk.WithBranch(g, br, omnisdk.NewGate("b", "none", "n")); err == nil {
		t.Error("a gate from a missing arm was accepted")
	}
	if _, err := omnisdk.WithBranch(g, br, omnisdk.NewGate("b", "all", "missing")); err == nil {
		t.Error("a gate to a missing node was accepted")
	}
}
