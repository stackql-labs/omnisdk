package omnisdk_test

import (
	"context"
	"fmt"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExecRunsOnlyNamedMethodAndRejectsMissingInputs(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/widgets" || r.URL.Query().Get("id") != "x" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"x","size":"large"}]`)
	}))
	defer server.Close()
	address := "stackql_unstable_widgets.things.widgets"
	signature, err := omnisdk.DescribeMethod(authRegistry, address, "find")
	if err != nil || signature.Method() != "find" {
		t.Fatalf("signature=%v err=%v", signature, err)
	}
	for _, scenario := range []struct {
		method    string
		params    map[string]string
		wantError bool
	}{
		{"find", map[string]string{"id": "x"}, false},
		{"find", nil, true},
		{"missing", map[string]string{"id": "x"}, true},
		{"", nil, true},
	} {
		graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewExecNode("exec", address, scenario.method, scenario.params, nil)}, nil)
		if err != nil {
			if !scenario.wantError {
				t.Fatal(err)
			}
			continue
		}
		plan, err := omnisdk.NewGraphQuery(authRegistry, graph, omnisdk.Args{Endpoint: server.URL})
		if scenario.wantError {
			if err == nil {
				t.Errorf("accepted invalid EXEC %q", scenario.method)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		rows, err := plan.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !rows.Next() || rows.Row()["id"] != "x" {
			t.Fatalf("row=%v error=%v", rows.Row(), rows.Err())
		}
		for rows.Next() {
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	if calls != 1 {
		t.Fatalf("made %d requests, want exactly 1", calls)
	}
}
