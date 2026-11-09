package omnisdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

func TestSQLTxnRollbackLocksOnlyMutationScope(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	log, err := ledger.NewFile(filepath.Join(state, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	leaser := lease.NewLeaser(log, nil)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if _, err := leaser.Acquire(ctx, "query", lease.Using([]facade.LedgerKey{"query/adornment"}, []facade.LedgerKey{"query/widget"}), "other", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
			t.Errorf("query target not exclusively locked before request: %v", err)
		}
		unrelated, err := leaser.Acquire(ctx, "query", lease.Of("query/unrelated"), "other", time.Minute)
		if err != nil {
			t.Errorf("unrelated query resource blocked: %v", err)
		} else {
			_ = unrelated.Release(ctx)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"w-1","size":"M"}`)
	}))
	defer server.Close()
	write, err := omnisdk.DescribeMutation(authRegistry, widgets, "insert")
	if err != nil {
		t.Fatal(err)
	}
	sql, err := query.NewMutation(query.NewInsert(query.NewResource("w", "widgets.things.widgets"), query.NewAssignment("size", query.NewLiteral("M"))), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	statement := omnisdk.SQLStatement(sql, map[string]omnisdk.Table{"w": write}, omnisdk.NewManaged("widget", []string{"w"}, "id", "id"))
	plan, err := omnisdk.Prepare(authRegistry, statement, omnisdk.NewLedger("query", state, "insert"), omnisdk.Args{Endpoint: server.URL, TxnRollback: true})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, plan)
	if requests.Load() != 1 || len(rows) != 1 {
		t.Fatalf("requests=%d rows=%v", requests.Load(), rows)
	}
	after, err := leaser.Acquire(ctx, "query", lease.Of("query/widget"), "after", time.Minute)
	if err != nil {
		t.Fatal("query commit leaked lock:", err)
	}
	_ = after.Release(ctx)
	if _, err := omnisdk.Prepare(authRegistry, omnisdk.SQLStatement(sql, map[string]omnisdk.Table{"w": write}), omnisdk.NoWAL(), omnisdk.Args{Endpoint: server.URL, TxnRollback: true}); err == nil {
		t.Fatal("unscoped rollback accepted")
	}
	if requests.Load() != 1 {
		t.Fatal("unscoped rollback dispatched a mutation")
	}
}

func TestRollbackPreventsAdornmentThroughUndo(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%t", rollback), func(t *testing.T) {
			ctx := context.Background()
			state := t.TempDir()
			log, err := ledger.NewFile(filepath.Join(state, "ledger"))
			if err != nil {
				t.Fatal(err)
			}
			leaser := lease.NewLeaser(log, nil)
			var creates, deletes atomic.Int32
			check := func() {
				t.Helper()
				claim, err := leaser.Acquire(ctx, "demo", lease.Using([]facade.LedgerKey{"demo/adornment"}, []facade.LedgerKey{"demo/first"}), "adornment", time.Minute)
				if rollback {
					if !errors.Is(err, facade.ErrLeaseHeld) {
						t.Errorf("adornment admitted during compensatable execution: %v", err)
					}
				} else if err != nil {
					t.Errorf("non-rollback execution retained exclusive protection: %v", err)
				}
				if claim != nil {
					_ = claim.Release(ctx)
				}
				independent, err := leaser.Acquire(ctx, "demo", lease.Of("demo/independent"), "other", time.Minute)
				if err != nil {
					t.Errorf("whole collection was locked: %v", err)
				} else {
					_ = independent.Release(ctx)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					creates.Add(1)
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["size"] == "bad" {
						check()
						w.WriteHeader(400)
						fmt.Fprint(w, `{"error":"refused"}`)
						return
					}
					fmt.Fprint(w, `{"id":"first-1","size":"L"}`)
				} else if r.Method == http.MethodDelete {
					check()
					deletes.Add(1)
					w.WriteHeader(204)
				} else {
					t.Errorf("unexpected method %s", r.Method)
				}
			}))
			defer server.Close()
			graph, err := omnisdk.NewGraph([]omnisdk.Node{
				omnisdk.NewMutationNode("first", widgets, "insert", nil, nil, map[string]any{"size": "L"}),
				omnisdk.NewMutationNode("fail", widgets, "insert", nil, nil, map[string]any{"size": "bad", "color": nil}),
			}, []omnisdk.Wiring{omnisdk.NewWiring("fail", []omnisdk.Inbound{omnisdk.NewInbound("first", "id", "color")}, "", "")})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := omnisdk.ConvergeGraph(authRegistry, "demo", state, "run", graph, []omnisdk.Managed{omnisdk.NewManaged("first", []string{"first"}, "id", "id"), omnisdk.NewManaged("failed", []string{"fail"}, "id", "id")}, omnisdk.Args{Endpoint: server.URL, IACRollback: rollback})
			if err != nil {
				t.Fatal(err)
			}
			rows := drain(t, plan)
			last := rows[len(rows)-1]
			wantDelete := int32(0)
			if rollback {
				wantDelete = 1
			}
			if creates.Load() != 2 || deletes.Load() != wantDelete || !strings.HasPrefix(fmt.Sprint(last["status"]), "failed") {
				t.Fatalf("creates=%d deletes=%d rows=%v", creates.Load(), deletes.Load(), rows)
			}
		})
	}
}
