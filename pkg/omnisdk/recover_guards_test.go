package omnisdk

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func TestRecoveryRejectsStalePriorStateAndDependents(t *testing.T) {
	for _, scenario := range []string{"stale", "prior", "dependent", "diagnostic", "busy"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			state := t.TempDir()
			log, err := ledger.NewFile(filepath.Join(state, "ledger"))
			if err != nil {
				t.Fatal(err)
			}
			key := facade.LedgerKey("demo/item")
			if err := log.Begin(ctx, key, []byte(`{"name":"new"}`), facade.LedgerVersionNone); err != nil {
				t.Fatal(err)
			}
			_, version, _, err := log.Get(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "prior" {
				if err := log.Resolve(ctx, key, []byte("previous-id"), []byte(`{"provider":"widgets","address":"things.widgets"}`), version); err != nil {
					t.Fatal(err)
				}
				_, version, _, _ = log.Get(ctx, key)
				if err := log.Begin(ctx, key, []byte(`{"name":"changed"}`), version); err != nil {
					t.Fatal(err)
				}
				_, version, _, _ = log.Get(ctx, key)
			}
			if scenario == "dependent" {
				child := facade.LedgerKey("demo/child")
				if err := log.Begin(ctx, child, []byte(`{}`), facade.LedgerVersionNone); err != nil {
					t.Fatal(err)
				}
				_, v, _, _ := log.Get(ctx, child)
				metadata, _ := json.Marshal(aboutRecord{Provider: "widgets", Address: "things.widgets", DependsOn: []string{string(key)}})
				if err := log.Resolve(ctx, child, []byte("child-id"), metadata, v); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "diagnostic" {
				if err := os.MkdirAll(filepath.Join(state, "journal", "recover.diagnostics.jsonl"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "busy" {
				held, err := lease.NewLeaser(log, nil).Acquire(ctx, "demo", lease.Of(key), "other", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				defer held.Release(ctx)
			}
			submitted := string(version)
			if scenario == "stale" {
				submitted = "outdated"
			}
			_, err = (ledgerWAL{collection: "demo", state: state, runID: "recover"}).Recover(ctx, "item", "absent", "", submitted, Args{})
			if err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			entry, after, found, err := log.Get(ctx, key)
			if err != nil || !found || after != version || entry.Phase() != facade.LedgerPending {
				t.Fatal("failed recovery changed resource", err)
			}
		})
	}
}
