package omnisdk_test

import (
	"context"
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
