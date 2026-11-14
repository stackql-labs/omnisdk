package omnisdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/public/authorization"
	"github.com/stackql-labs/omnisdk/public/principal"
)

func TestTransactionRetainsIaCLocksAndCompensatesAfterResume(t *testing.T) {
	registry := transactionRegistry(t)
	ctx := context.Background()
	caller, _ := accessContractCaller(t, true)
	session, err := omnisdk.NewTransactionSession(caller)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	config := omnisdk.TransactionConfig{Collection: "sql", State: t.TempDir(), DynamicScope: true}
	handle, err := session.Begin(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	log, err := ledger.NewFile(filepath.Join(config.State, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	leaser := lease.NewLeaser(log, nil)
	var creates, deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := leaser.Acquire(ctx, "sql", lease.Of("sql/widget"), "other", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
			t.Errorf("mutation not locked: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "DELETE" {
			deletes.Add(1)
			fmt.Fprint(w, `{}`)
			return
		}
		creates.Add(1)
		fmt.Fprint(w, `{"id":"w-1","size":"M"}`)
	}))
	defer server.Close()
	graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("create", widgets, "insert", map[string]string{"size": "M"}, nil, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	args := omnisdk.Args{Endpoint: server.URL}
	rows, err := session.Execute(ctx, handle, registry, graph, []omnisdk.Managed{omnisdk.NewManaged("widget", []string{"create"}, "id", "id")}, args)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if creates.Load() != 1 {
		t.Fatal("create not executed", creates.Load())
	}
	if _, err := leaser.Acquire(ctx, "sql", lease.Of("sql/widget"), "other", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("transaction released mutation lock before completion", err)
	}
	unrelated, err := leaser.Acquire(ctx, "sql", lease.Of("sql/unrelated"), "other", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_ = unrelated.Release(ctx)
	other, err := omnisdk.NewTransactionSession(caller)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Commit(ctx, handle); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("foreign session used handle", err)
	}
	if _, err := other.Resume(ctx, config, handle.ID()); err == nil {
		t.Fatal("resumed active writer")
	}
	session.Close()
	recovered, err := other.Resume(ctx, config, handle.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Rollback(ctx, recovered, registry, args); err != nil {
		t.Fatal(err)
	}
	if deletes.Load() != 1 {
		t.Fatal("persisted inverse not executed", deletes.Load())
	}
	status, err := omnisdk.InspectTransaction(ctx, config, handle.ID(), omnisdk.Args{Caller: caller})
	if err != nil || status != "rolled_back" {
		t.Fatal(status, err)
	}
	if _, err := other.Resume(ctx, config, handle.ID()); err == nil {
		t.Fatal("closed transaction resumed")
	}
}

func TestTransactionRejectsIrreversibleMutationBeforeDispatch(t *testing.T) {
	caller, _ := accessContractCaller(t, true)
	session, err := omnisdk.NewTransactionSession(caller)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	config := omnisdk.TransactionConfig{Collection: "sql", State: t.TempDir(), Keys: []string{"widget"}}
	handle, err := session.Begin(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("delete", widgets, "delete", map[string]string{"id": "w-1"}, nil, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Execute(context.Background(), handle, authRegistry, graph, []omnisdk.Managed{omnisdk.NewManaged("widget", []string{"delete"}, "id", "id")}, omnisdk.Args{Endpoint: server.URL}); err == nil {
		t.Fatal("irreversible delete accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("irreversible request dispatched")
	}
	if err := session.Rollback(context.Background(), handle, authRegistry, omnisdk.Args{}); err != nil {
		t.Fatal(err)
	}
}

func TestTransactionAuthorizationPrecedesPersistence(t *testing.T) {
	if _, err := omnisdk.NewTransactionSession(nil); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal(err)
	}
	caller, _ := accessContractCaller(t, false)
	session, err := omnisdk.NewTransactionSession(caller)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Begin(context.Background(), omnisdk.TransactionConfig{Collection: "sql", State: t.TempDir()}); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal(err)
	}
}

func TestTransactionDeferredQueueSurvivesResumeAndChecksFreshPolicy(t *testing.T) {
	ctx := context.Background()
	caller, _ := accessContractCaller(t, true)
	session, _ := omnisdk.NewTransactionSession(caller)
	config := omnisdk.TransactionConfig{Mode: omnisdk.TransactionLazy, Collection: "sql", State: t.TempDir()}
	handle, err := session.Begin(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Queue(ctx, handle, []byte(`{"sql":"first"}`)); err != nil {
		t.Fatal(err)
	}
	if err = session.Queue(ctx, handle, []byte(`{"sql":"second"}`)); err != nil {
		t.Fatal(err)
	}
	if err = session.Commit(ctx, handle); err == nil {
		t.Fatal("committed unexecuted statements")
	}
	denied, _ := accessContractCaller(t, false)
	if err = session.Commit(denied.Context(ctx), handle); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("fresh policy ignored", err)
	}
	session.Close()
	resumed, _ := omnisdk.NewTransactionSession(caller)
	defer resumed.Close()
	handle, err = resumed.Resume(ctx, config, handle.ID())
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	if err = resumed.Drain(ctx, handle, func(ctx context.Context, payload json.RawMessage) error {
		var item struct{ SQL string }
		if err := json.Unmarshal(payload, &item); err != nil {
			return err
		}
		order = append(order, item.SQL)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(order) != "[first second]" {
		t.Fatal(order)
	}
	if err = resumed.Commit(ctx, handle); err != nil {
		t.Fatal(err)
	}
}

func transactionRegistry(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	provider := "widgets/v00.00.00000/provider.yaml"
	service := "widgets/v00.00.00000/services/things.yaml"
	for _, path := range []string{provider, service} {
		raw, err := os.ReadFile(filepath.Join(authRegistry, path))
		if err != nil {
			t.Fatal(err)
		}
		if path == service {
			raw = []byte(strings.Replace(string(raw), "        create:\n", "        create:\n          inverse:\n            sqlVerb: {$ref: '#/components/x-stackQL-resources/widgets/methods/delete'}\n            tokens:\n              id: {key: id, location: body, algorithm: toString}\n", 1))
		}
		target := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestTransactionRecoveryRequiresVerifiedVersionAndNeverReplaysCreate(t *testing.T) {
	ctx := context.Background()
	registry := transactionRegistry(t)
	caller, _ := accessContractCaller(t, true)
	session, _ := omnisdk.NewTransactionSession(caller)
	config := omnisdk.TransactionConfig{Collection: "recovery", State: t.TempDir(), Keys: []string{"widget"}}
	handle, err := session.Begin(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	var creates, deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "DELETE" {
			deletes.Add(1)
			fmt.Fprint(w, `{}`)
			return
		}
		creates.Add(1)
		fmt.Fprint(w, `{"size":"M"}`) // provider created it but omitted the handle
	}))
	defer server.Close()
	graph, _ := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("create", widgets, "insert", map[string]string{"size": "M"}, nil, nil)}, nil)
	args := omnisdk.Args{Endpoint: server.URL}
	if _, err = session.Execute(ctx, handle, registry, graph, []omnisdk.Managed{omnisdk.NewManaged("widget", []string{"create"}, "id", "id")}, args); err == nil {
		t.Fatal("missing identity accepted")
	}
	session.Close()
	recovered, _ := omnisdk.NewTransactionSession(caller)
	defer recovered.Close()
	handle, err = recovered.Resume(ctx, config, handle.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err = recovered.Commit(ctx, handle); err == nil {
		t.Fatal("committed uncertain create")
	}
	if err = recovered.Rollback(ctx, handle, registry, args); err == nil {
		t.Fatal("blindly compensated uncertain create")
	}
	snapshot, err := recovered.Status(ctx, handle)
	if err != nil || len(snapshot.Pending) != 1 {
		t.Fatal(snapshot, err)
	}
	decision := omnisdk.TransactionRecovery{Version: snapshot.Version, Resources: []omnisdk.TransactionResolution{{Key: "widget", Outcome: "applied", Version: "stale", Identity: "w-verified"}}}
	if err = recovered.Recover(ctx, handle, decision); !errors.Is(err, facade.ErrLedgerConflict) {
		t.Fatal("stale recovery accepted", err)
	}
	decision.Resources[0].Version = snapshot.Pending[0].Version
	if err = recovered.Recover(ctx, handle, decision); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 || deletes.Load() != 0 {
		t.Fatal("recovery replayed an effect", creates.Load(), deletes.Load())
	}
	if err = recovered.Rollback(ctx, handle, registry, args); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 || deletes.Load() != 1 {
		t.Fatal("verified inverse did not execute exactly once", creates.Load(), deletes.Load())
	}
}

func TestTransactionOldCapabilityCannotReviveOnSameSessionResume(t *testing.T) {
	caller, _ := accessContractCaller(t, true)
	session, _ := omnisdk.NewTransactionSession(caller)
	defer session.Close()
	cfg := omnisdk.TransactionConfig{Collection: "tokens", State: t.TempDir()}
	old, err := session.Begin(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	session.Close()
	fresh, err := session.Resume(context.Background(), cfg, old.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Commit(context.Background(), old); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("old capability revived", err)
	}
	if err = session.Commit(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
}

func TestTransactionCreateWithoutDeclaredInverseIsRejected(t *testing.T) {
	caller, _ := accessContractCaller(t, true)
	session, _ := omnisdk.NewTransactionSession(caller)
	defer session.Close()
	handle, err := session.Begin(context.Background(), omnisdk.TransactionConfig{Collection: "inverse", State: t.TempDir(), Keys: []string{"widget"}})
	if err != nil {
		t.Fatal(err)
	}
	graph, _ := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("create", widgets, "insert", map[string]string{"size": "M"}, nil, nil)}, nil)
	if _, err = session.Execute(context.Background(), handle, authRegistry, graph, []omnisdk.Managed{omnisdk.NewManaged("widget", []string{"create"}, "id", "id")}, omnisdk.Args{}); err == nil || !strings.Contains(err.Error(), "declared inverse") {
		t.Fatal(err)
	}
	if err = session.Commit(context.Background(), handle); err == nil {
		t.Fatal("committed aborted transaction")
	}
}

func TestTransactionConcurrentSessionsCannotKidnapAnotherPrincipal(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	probe := &permissionProbe{allow: true, calls: map[string]int{}}
	callers := []authorization.Session{contractSession(t, probe)}
	for _, name := range []string{"bob", "carol"} {
		data := fmt.Sprintf(`{"principals":[{"id":%q,"identities":[{"issuer":"test","subject":%q}]}]}`, name, name)
		directory, err := principal.NewMemoryDirectoryFromBytes([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		identity, _ := principal.NewIdentity("test", name)
		caller, err := authorization.NewSession(ctx, identity, directory, probe)
		if err != nil {
			t.Fatal(err)
		}
		callers = append(callers, caller)
	}
	type result struct {
		session   *omnisdk.TransactionSession
		handle    omnisdk.TransactionHandle
		principal int
		err       error
	}
	start := make(chan struct{})
	results := make(chan result, 10)
	for i := 0; i < 10; i++ {
		go func(index int) {
			session, err := omnisdk.NewTransactionSession(callers[index%3])
			if err != nil {
				results <- result{err: err}
				return
			}
			<-start
			handle, err := session.Begin(ctx, omnisdk.TransactionConfig{Collection: "concurrent", State: state, Keys: []string{"shared"}})
			results <- result{session, handle, index % 3, err}
		}(i)
	}
	close(start)
	var winner result
	wins := 0
	for i := 0; i < 10; i++ {
		r := <-results
		if r.session != nil {
			defer r.session.Close()
		}
		if r.err == nil {
			winner = r
			wins++
		}
	}
	if wins != 1 {
		t.Fatal("exclusive claims admitted", wins)
	}
	foreign, _ := omnisdk.NewTransactionSession(callers[(winner.principal+1)%3])
	defer foreign.Close()
	if err := foreign.Commit(ctx, winner.handle); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("foreign session used a handle", err)
	}
	winner.session.Close()
	if _, err := foreign.Resume(ctx, omnisdk.TransactionConfig{Collection: "concurrent", State: state}, winner.handle.ID()); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("foreign principal resumed transaction", err)
	}
}
