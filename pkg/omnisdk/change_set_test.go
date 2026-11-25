package omnisdk_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestIaCChangeSet(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{}}
	server := store.serve(t)
	defer server.Close()
	ctx := context.Background()
	args := omnisdk.Args{Endpoint: server.URL}
	state := filepath.Join(t.TempDir(), "state")
	resources, err := omnisdk.ParseResources([]byte(`[{"key":"widget","provider":"widgets","address":"things.widgets","identity":"id","addressed_by":"id","desired":{"size":"S","color":"red"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	preview := func(resources []omnisdk.ManagedResource) omnisdk.ChangeSet {
		t.Helper()
		set, err := omnisdk.Preview(ctx, authRegistry, omnisdk.DesiredState(resources), omnisdk.NewLedger("widgets", state, "preview"), args)
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	initial := preview(resources)
	if calls := store.took(); initial.Changes[0].Action != "create" || len(calls) != 0 {
		t.Fatalf("plan=%+v calls=%v", initial, calls)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("preview created state: %v", err)
	}
	raw, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip omnisdk.ChangeSet
	if err := json.Unmarshal(raw, &roundtrip); err != nil {
		t.Fatal(err)
	}
	apply := func(set omnisdk.ChangeSet, name string) (omnisdk.Rows, error) {
		t.Helper()
		p, err := omnisdk.ApplyChangeSet(authRegistry, set, omnisdk.NewLedger("widgets", state, name), args)
		if err != nil {
			t.Fatal(err)
		}
		return p.Open(ctx)
	}
	rows, err := apply(roundtrip, "apply")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() || rows.Row()["status"] != "applied" {
		t.Fatalf("apply: row=%v err=%v", rows.Row(), rows.Err())
	}
	if _, err := apply(roundtrip, "stale"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale plan error: %v", err)
	}
	noop := preview(resources)
	if noop.Changes[0].Action != "no-op" {
		t.Fatalf("plan=%+v", noop)
	}
	store.mu.Lock()
	store.items["w-1"]["color"] = "blue"
	store.mu.Unlock()
	if _, err := apply(noop, "drift"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("drift error: %v", err)
	}
	update := preview(resources)
	if update.Changes[0].Action != "update" {
		t.Fatalf("plan=%+v", update)
	}
	store.mu.Lock()
	store.items["w-1"]["color"] = "green"
	store.mu.Unlock()
	if _, err := apply(update, "changed-drift"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed drift error: %v", err)
	}
	update = preview(resources)
	rows, err = apply(update, "update")
	if err != nil {
		t.Fatal(err)
	}
	if result := readChangeSetRows(t, rows); len(result) != 1 || result[0]["status"] != "applied" {
		t.Fatalf("update rows=%v", result)
	}
	if next := preview(resources); next.Changes[0].Action != "no-op" {
		t.Fatalf("plan after update=%+v", next)
	}

}

func TestIaCChangeSetDefersNewDependencyIdentity(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{}}
	server := store.serve(t)
	defer server.Close()
	ctx := context.Background()
	args := omnisdk.Args{Endpoint: server.URL}
	state := t.TempDir()
	resources, err := omnisdk.ParseResources([]byte(`[
		{"key":"child","provider":"widgets","address":"things.widgets","identity":"id","addressed_by":"id","inbound":[{"from":"parent","as":"size"}],"desired":{"color":"blue"}},
		{"key":"parent","provider":"widgets","address":"things.widgets","identity":"id","addressed_by":"id","desired":{"size":"S","color":"red"}}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	wal := omnisdk.NewLedger("widgets", state, "apply")
	set, err := omnisdk.Preview(ctx, authRegistry, omnisdk.DesiredState(resources), wal, args)
	if err != nil {
		t.Fatal(err)
	}
	if set.Changes[0].Action != "unknown" || set.Changes[0].DependsOn[0] != "parent" || set.Changes[1].Action != "create" {
		t.Fatalf("changes=%+v", set.Changes)
	}
	if calls := store.took(); len(calls) != 0 {
		t.Fatalf("preview requests=%v", calls)
	}
	p, err := omnisdk.ApplyChangeSet(authRegistry, set, wal, args)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := p.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result := readChangeSetRows(t, rows); len(result) != 2 {
		t.Fatalf("apply rows=%v", result)
	}
	store.mu.Lock()
	if got := store.items["w-2"]["size"]; got != "w-1" {
		t.Errorf("child did not receive parent identity: %v", got)
	}
	store.mu.Unlock()
	set, err = omnisdk.Preview(ctx, authRegistry, omnisdk.DesiredState(resources), wal, args)
	if err != nil {
		t.Fatal(err)
	}
	if set.Changes[0].Action != "no-op" || set.Changes[1].Action != "no-op" {
		t.Fatalf("changes=%+v", set.Changes)
	}
}

func readChangeSetRows(t *testing.T, rows omnisdk.Rows) []omnisdk.Row {
	t.Helper()
	defer rows.Close()
	var result []omnisdk.Row
	for rows.Next() {
		row := rows.Row()
		if row["error"] != nil {
			t.Fatalf("execution failed: %v", row)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestIaCExplicitLookup(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{"existing": {"id": "existing", "size": "S", "color": "red"}}}
	server := store.serve(t)
	defer server.Close()
	args := omnisdk.Args{Endpoint: server.URL}
	wal := omnisdk.NewLedger("widgets", t.TempDir(), "lookup")
	resources, err := omnisdk.ParseResources([]byte(`[{"key":"widget","provider":"widgets","address":"things.widgets","addressed_by":"id","identity":"id","lookup_identity":"existing","desired":{"size":"S"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	preview := func() (omnisdk.ChangeSet, error) {
		return omnisdk.Preview(ctx, authRegistry, omnisdk.DesiredState(resources), wal, args)
	}
	plan, err := preview()
	if err != nil || plan.Changes[0].Action != "no-op" {
		t.Fatalf("initial plan: %+v %v", plan, err)
	}
	store.items["existing"]["size"] = "M"
	plan, err = preview()
	if err != nil || plan.Changes[0].Action != "update" {
		t.Fatalf("drift: %+v %v", plan, err)
	}
	pl, err := omnisdk.ApplyChangeSet(authRegistry, plan, wal, args)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pl.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	readChangeSetRows(t, rows)
	if got := store.items["existing"]; got["size"] != "S" || got["color"] != "red" {
		t.Fatalf("after update: %v", got)
	}
	plan, err = preview()
	if err != nil || plan.Changes[0].Action != "no-op" {
		t.Fatalf("repeat: %+v %v", plan, err)
	}
	delete(store.items, "existing")
	if _, err := preview(); err == nil {
		t.Fatal("missing lookup target must fail")
	}
	var writes []string
	for _, call := range store.took() {
		if call != "find" {
			writes = append(writes, call)
		}
	}
	if len(writes) != 1 || writes[0] != "update" {
		t.Fatalf("writes: %v", writes)
	}
}
