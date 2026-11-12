package omnisdk_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestDestroyRecoversMissingMetadataFromMutationJournal(t *testing.T) {
	ctx := context.Background()
	store := &widgetStore{items: map[string]map[string]any{}}
	srv := store.serve(t)
	defer srv.Close()
	state := t.TempDir()
	convergeWidgets(t, srv, state, map[string]map[string]any{"widget": {"size": "S", "color": "red"}}, []string{"widget"})
	log, err := ledger.NewFile(filepath.Join(state, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	entry, ver, found, err := log.Get(ctx, "demo/widget")
	if err != nil || !found {
		t.Fatalf("entry found=%v err=%v", found, err)
	}
	// Recreate a legacy live entry without About; retain the original forward journal.
	if err := log.Forget(ctx, entry.Key(), ver); err != nil {
		t.Fatal(err)
	}
	if err := log.Begin(ctx, entry.Key(), entry.Plan(), ""); err != nil {
		t.Fatal(err)
	}
	_, ver, _, err = log.Get(ctx, entry.Key())
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Resolve(ctx, entry.Key(), entry.Identity(), nil, ver); err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.Prepare(authRegistry, omnisdk.Destroy(), omnisdk.NewLedger("demo", state, "destroy-recovered"), omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, pl)
	if len(rows) != 1 || rows[0]["status"] != "destroyed" {
		t.Fatalf("rows=%v", rows)
	}
	if len(store.items) != 0 {
		t.Fatalf("objects remain: %v", store.items)
	}
}

func TestCompletedLedgerSurvivesJournalAndDiagnosticsRemoval(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{}}
	srv := store.serve(t)
	defer srv.Close()
	state := t.TempDir()
	resource := omnisdk.NewResource("widget", "widgets", "things.widgets", []byte(`{"size":"S","color":"red"}`), nil, nil, "", "", "id", "id", "")
	for _, run := range []string{"initial", "reapply"} {
		pl, err := omnisdk.Prepare(authRegistry, omnisdk.DesiredState([]omnisdk.ManagedResource{resource}), omnisdk.NewLedger("demo", state, run), omnisdk.Args{Endpoint: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		rows := drain(t, pl)
		if len(rows) != 1 || rows[0]["status"] != "applied" {
			t.Fatalf("converge did not complete: %v", rows)
		}
		// Simulate collection of all completed journals, including diagnostics.
		if err := os.RemoveAll(filepath.Join(state, "journal")); err != nil {
			t.Fatal(err)
		}
	}
	calls := store.took()
	creates := 0
	for _, call := range calls {
		if call == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("reapply duplicated the resource: %v", calls)
	}
	pl, err := omnisdk.Prepare(authRegistry, omnisdk.Destroy(), omnisdk.NewLedger("demo", state, "destroy-without-history"), omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, pl)
	if len(rows) != 1 || rows[0]["status"] != "destroyed" || len(store.items) != 0 {
		t.Fatalf("destroy depended on completed history: rows=%v objects=%v", rows, store.items)
	}
}
