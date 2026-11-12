package omnisdk_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/public/authorization"
	"github.com/stackql-labs/omnisdk/public/principal"
)

type permissionProbe struct {
	mu    sync.Mutex
	allow bool
	calls map[string]int
}

func (p *permissionProbe) Authorize(ctx context.Context, _ principal.Principal, resource, action string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[resource+":"+action]++
	if !p.allow {
		return authorization.ErrDenied
	}
	return nil
}
func accessContractCaller(t *testing.T, allow bool) (authorization.Session, *permissionProbe) {
	t.Helper()
	probe := &permissionProbe{allow: allow, calls: map[string]int{}}
	return contractSession(t, probe), probe
}

func contractSession(t *testing.T, authorizer authorization.Authorizer) authorization.Session {
	t.Helper()
	directory, err := principal.NewMemoryDirectoryFromBytes([]byte(`{"principals":[{"id":"alice","identities":[{"issuer":"test","subject":"alice"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := principal.NewIdentity("test", "alice")
	if err != nil {
		t.Fatal(err)
	}
	session, err := authorization.NewSession(context.Background(), identity, directory, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

type accessContractPlan func(context.Context) (omnisdk.Rows, error)

func (p accessContractPlan) Open(ctx context.Context) (omnisdk.Rows, error) { return p(ctx) }

func TestQueryableAuthorizationFailsClosedBeforeExecution(t *testing.T) {
	caller, probe := accessContractCaller(t, true)
	var opened atomic.Int32
	inner := accessContractPlan(func(context.Context) (omnisdk.Rows, error) { opened.Add(1); return &handleRows{}, nil })
	for _, permissions := range [][]authorization.Permission{nil, {{Resource: " ", Action: "read"}}, {{Action: "read"}}, {{Resource: "collection/demo"}}, {{Resource: "collection/demo", Action: "read"}, {Action: "read"}}} {
		plan := omnisdk.WithPermissions(inner, omnisdk.Args{Caller: caller, RequireCaller: true}, permissions...)
		if _, err := plan.Open(context.Background()); !errors.Is(err, authorization.ErrDenied) {
			t.Fatalf("unclassified execution: %v", err)
		}
	}
	if opened.Load() != 0 || len(probe.calls) != 0 {
		t.Fatal("classification failure reached policy or execution", probe.calls)
	}
	legacy := omnisdk.WithPermissions(inner, omnisdk.Args{})
	rows, err := legacy.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if opened.Load() != 1 {
		t.Fatal("legacy execution changed")
	}
}

func TestQueryableAuthorizationInheritsExactGrantsAndResetsForNewCaller(t *testing.T) {
	allowed, probe := accessContractCaller(t, true)
	denied, _ := accessContractCaller(t, false)
	var opened atomic.Int32
	permission := authorization.Permission{Resource: "collection/demo", Action: "read"}
	leaf := accessContractPlan(func(context.Context) (omnisdk.Rows, error) { opened.Add(1); return &handleRows{}, nil })
	inner := omnisdk.WithPermissions(leaf, omnisdk.Args{}, permission)
	outer := omnisdk.WithPermissions(inner, omnisdk.Args{}, permission)
	rows, err := outer.Open(allowed.Context(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if probe.calls["collection/demo:read"] != 1 || opened.Load() != 1 {
		t.Fatal("nested gate duplicated authorization", probe.calls)
	}
	switchCaller := accessContractPlan(func(ctx context.Context) (omnisdk.Rows, error) { return inner.Open(denied.Context(ctx)) })
	protected := omnisdk.WithPermissions(switchCaller, omnisdk.Args{}, permission)
	if _, err := protected.Open(allowed.Context(context.Background())); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("old caller's grant leaked", err)
	}
	if opened.Load() != 1 {
		t.Fatal("denied caller reached inner plan")
	}
}

func TestDirectRelationAndLedgerReadsUseTheCommonGate(t *testing.T) {
	caller, _ := accessContractCaller(t, false)
	ctx := caller.Context(context.Background())
	var opened atomic.Int32
	relation, err := omnisdk.NewRelation("local.inventory", nil, nil, func(context.Context, map[string]any) (omnisdk.Rows, error) { opened.Add(1); return &handleRows{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relation.Open(ctx, nil); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("relation bypass", err)
	}
	root := filepath.Join(t.TempDir(), "not-created")
	if _, err := omnisdk.ReadLedger(ctx, "demo", root); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("ledger bypass", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("denial touched storage", err)
	}
	if opened.Load() != 0 {
		t.Fatal("denied relation callback ran")
	}
}

func TestRecoveryAuthorizationPrecedesStateAndJournalCreation(t *testing.T) {
	caller, _ := accessContractCaller(t, false)
	state := filepath.Join(t.TempDir(), "not-created")
	plan, err := omnisdk.Prepare("", omnisdk.Recover("vm", "absent", "", "1"), omnisdk.NewLedger("demo", state, "denied"), omnisdk.Args{Caller: caller, RequireCaller: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Open(context.Background()); !errors.Is(err, authorization.ErrDenied) {
		t.Fatal("recovery bypass", err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("denied recovery created state", err)
	}
}

func TestSleepUsesTheCommonGate(t *testing.T) {
	caller, _ := accessContractCaller(t, false)
	plan, err := omnisdk.NewSleep(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := plan.Open(caller.Context(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() || !errors.Is(rows.Err(), authorization.ErrDenied) {
		t.Fatal("sleep bypass", rows.Err())
	}
}

func TestConditionalAuthorizationRequiresOnlyExecutedArms(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{}}
	server := store.serve(t)
	defer server.Close()
	state := t.TempDir()
	apply := func(run, color string, caller authorization.Session) []omnisdk.Row {
		t.Helper()
		resources := []omnisdk.ManagedResource{omnisdk.NewResource("vm", "widgets", "things.widgets", []byte(`{"size":"S","color":"`+color+`"}`), nil, nil, "", "", "id", "id", "")}
		plan, err := omnisdk.Prepare(authRegistry, omnisdk.DesiredState(resources), omnisdk.NewLedger("demo", state, run), omnisdk.Args{Endpoint: server.URL, Caller: caller})
		if err != nil {
			t.Fatal(err)
		}
		return readPlan(t, plan)
	}
	apply("initial", "red", nil)
	store.took()
	authorizer, err := authorization.NewCasbin([]authorization.Rule{
		{Subject: "user:alice", Resource: "collection/demo", Action: "converge"},
		{Subject: "user:alice", Resource: "operation/widgets/find/find_widget", Action: "execute"},
		{Subject: "user:alice", Resource: "credential/widgets", Action: "use"},
	})
	if err != nil {
		t.Fatal(err)
	}
	caller := contractSession(t, authorizer)
	unchanged := apply("no-op", "red", caller)
	for _, row := range unchanged {
		if row["error"] != nil {
			t.Fatal("unselected mutation required permission", row)
		}
	}
	if calls := store.took(); len(calls) != 1 || calls[0] != "find" {
		t.Fatal(calls)
	}
	changed := apply("denied-update", "blue", caller)
	if changed[len(changed)-1]["error"] == nil {
		t.Fatal("ungranted update succeeded", changed)
	}
	if calls := store.took(); len(calls) != 1 || calls[0] != "find" {
		t.Fatal("denied update reached provider", calls)
	}
	entries, err := omnisdk.ReadLedger(context.Background(), "demo", state)
	if err != nil || len(entries) != 1 || entries[0]["phase"] != "live" {
		t.Fatal("denied update changed ledger", entries, err)
	}
}

func TestConcurrentSessionsKeepPrincipalPermissionsAndRowsIsolated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	directory, err := principal.NewMemoryDirectoryFromBytes([]byte(`{"principals":[{"id":"alice","identities":[{"issuer":"test","subject":"alice"}]},{"id":"bob","identities":[{"issuer":"test","subject":"bob"}]},{"id":"carol","identities":[{"issuer":"test","subject":"carol"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"alice", "bob", "carol"}
	var rules []authorization.Rule
	var relations []omnisdk.Relation
	var arrived atomic.Int32
	ready := make(chan struct{})
	for _, name := range names {
		rules = append(rules, authorization.Rule{Subject: "user:" + name, Resource: "relation/local." + name, Action: "read"})
		relation, err := omnisdk.NewRelation("local."+name, nil, map[string]omnisdk.ColumnType{"owner": {Type: "string"}}, func(ctx context.Context, _ map[string]any) (omnisdk.Rows, error) {
			if arrived.Add(1) == 12 {
				close(ready)
			}
			select {
			case <-ready:
				return &handleRows{rows: []omnisdk.Row{{"owner": name}}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		relations = append(relations, relation)
	}
	policy, err := authorization.NewCasbin(rules)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := range 12 {
		owner := i % len(names)
		identity, err := principal.NewIdentity("test", names[owner])
		if err != nil {
			t.Fatal(err)
		}
		caller, err := authorization.NewSession(ctx, identity, directory, policy)
		if err != nil {
			t.Fatal(err)
		}
		graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("private", relations[owner].Address(), nil)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		planned, err := omnisdk.NewGraphQuery("not-a-registry", graph, omnisdk.Args{Caller: caller, RequireCaller: true, Relations: map[string]omnisdk.Relation{relations[owner].Address(): relations[owner]}})
		if err != nil {
			t.Fatal(err)
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			denied, err := relations[(owner+1)%3].Open(caller.Context(ctx), nil)
			if denied != nil {
				defer denied.Close()
			}
			if !errors.Is(err, authorization.ErrDenied) {
				t.Errorf("%s read another principal's relation: %v", names[owner], err)
				return
			}
			rows, err := planned.Open(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			defer rows.Close()
			if !rows.Next() || rows.Row()["owner"] != names[owner] || rows.Next() || rows.Err() != nil {
				t.Errorf("%s received incorrect private rows: %v", names[owner], rows.Err())
			}
		}()
	}
	workers.Wait()
	if arrived.Load() != 12 {
		t.Fatalf("expected 12 overlapping authorized queries, got %d", arrived.Load())
	}
}
