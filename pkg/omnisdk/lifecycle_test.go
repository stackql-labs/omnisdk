package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestLifecycleSelfReadWaitsAndCheckpointsHandle(t *testing.T) {
	registry := updateWidgetBundle(t, "      config:\n        lifecycle:\n          state_derivation: self_read_returns_data\n          interval: 1ms\n          max_attempts: 3\n")
	state := t.TempDir()
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			fmt.Fprint(w, `{"id":"w-1","size":"S"}`)
			return
		}
		if r.Method != "GET" || r.URL.Query().Get("id") != "w-1" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(400)
			return
		}
		handles, err := journal.AwaitHandles(context.Background(), filepath.Join(state, "journal"), "create")
		if err != nil || handles["demo/widget"].Inputs["id"] != "w-1" {
			t.Errorf("handle not durable before poll: %v %v", handles, err)
		}
		polls++
		if polls == 1 {
			http.Error(w, `{"error":"not yet visible"}`, 404)
			return
		}
		if polls == 2 {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, `[{"id":"w-1","size":"S"}]`)
	}))
	defer srv.Close()
	resource := omnisdk.NewResource("widget", "widgets", "things.widgets", []byte(`{"size":"S"}`), nil, nil, "", "", "id", "id", "")
	pl, err := omnisdk.Prepare(registry, omnisdk.DesiredState([]omnisdk.ManagedResource{resource}), omnisdk.NewLedger("demo", state, "create"), omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, pl)
	if polls != 3 || strings.Contains(fmt.Sprint(rows), "failed") {
		t.Fatalf("polls=%d rows=%v", polls, rows)
	}
}

func TestLifecycleDoesNotGuessSuccessOrRepeatCreate(t *testing.T) {
	for _, tc := range []struct {
		name, create, read string
		status, polls      int
	}{
		{"missing-handle", `{}`, `[]`, 200, 0},
		{"empty-observations", `{"id":"w-1"}`, `[]`, 200, 2},
		{"empty-object", `{"id":"w-1"}`, `{}`, 200, 2},
		{"forbidden", `{"id":"w-1"}`, `{"error":"forbidden"}`, 403, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := updateWidgetBundle(t, "      config:\n        lifecycle:\n          state_derivation: self_read_returns_data\n          interval: 1ms\n          max_attempts: 2\n")
			creates, polls := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "POST" {
					creates++
					fmt.Fprint(w, tc.create)
					return
				}
				polls++
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.read)
			}))
			defer srv.Close()
			resource := omnisdk.NewResource("widget", "widgets", "things.widgets", []byte(`{"size":"S"}`), nil, nil, "", "", "id", "id", "")
			pl, err := omnisdk.Prepare(registry, omnisdk.DesiredState([]omnisdk.ManagedResource{resource}), omnisdk.NewLedger("demo", t.TempDir(), "create"), omnisdk.Args{Endpoint: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			rows := drain(t, pl)
			if creates != 1 || polls != tc.polls || !strings.Contains(fmt.Sprint(rows), "unknown") {
				t.Fatalf("creates=%d polls=%d rows=%v", creates, polls, rows)
			}
		})
	}
}

func TestBetaIaCPersistsOperationHandle(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	view, err := omnisdk.EffectiveRegistry(corpus, []omnisdk.DocPatch{networksAwaitTheirOperation}, omnisdk.DocCache{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	api := &gcpNetworks{pollsUntilDone: 2}
	srv := api.serve(t)
	defer srv.Close()
	graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("net", networks, "insert", map[string]string{"project": "demo"}, nil, map[string]any{"name": "net-1"})}, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	pl, err := omnisdk.Prepare(view, omnisdk.GraphStatement(graph, omnisdk.NewManaged("net", []string{"net"}, "targetLink", "network")), omnisdk.NewLedger("demo", state, "create"), omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, pl)
	if strings.Contains(fmt.Sprint(rows), "failed") || api.polls != 2 {
		t.Fatalf("rows=%v polls=%d", rows, api.polls)
	}
	handles, err := journal.AwaitHandles(context.Background(), filepath.Join(state, "journal"), "create")
	if err != nil || handles["demo/net"].Inputs["operation"] != "op-1" || handles["demo/net"].Inputs["project"] != "demo" {
		t.Fatalf("handles=%v err=%v", handles, err)
	}
}

// Hosts with their own mutation completion must see the accepted create response,
// not an implicit self-read that can fail before the operation is polled.
func TestHostCompletionOwnsLifecycleWait(t *testing.T) {
	for _, hostCompletion := range []bool{false, true} {
		t.Run(fmt.Sprint(hostCompletion), func(t *testing.T) {
			registry := updateWidgetBundle(t, "      config:\n        lifecycle:\n          state_derivation: self_read_returns_data\n          interval: 1ms\n          max_attempts: 2\n")
			creates, reads, completions := 0, 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != http.MethodPost {
					reads++
					http.Error(w, "unexpected self read", http.StatusForbidden)
					return
				}
				creates++
				fmt.Fprint(w, `{"operation":"op-1"}`)
			}))
			defer srv.Close()
			graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("create", widgets, "insert", nil, nil, map[string]any{"size": "S"})}, nil)
			if err != nil {
				t.Fatal(err)
			}
			args := omnisdk.Args{Endpoint: srv.URL}
			args.MutationCompletion = completionFunc(func(_ context.Context, _, _ string, _ map[string]any, row omnisdk.Row) (omnisdk.Row, error) {
				completions++
				if row["operation"] != "op-1" {
					t.Errorf("completion lost operation response: %v", row)
				}
				if hostCompletion {
					return omnisdk.Row{"id": "w-1"}, nil
				}
				return row, nil
			})
			pl, err := omnisdk.NewGraphQuery(registry, graph, args)
			if err != nil {
				t.Fatal(err)
			}
			drain(t, pl)
			if creates != 1 || reads != 0 || completions != 1 {
				t.Fatalf("creates=%d reads=%d completions=%d", creates, reads, completions)
			}
		})
	}
}
