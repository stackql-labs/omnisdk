package omnisdk

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func TestRollbackRetainsMutationFootprint(t *testing.T) {
	ctx := context.Background()
	h, err := hold(ctx, t.TempDir(), "demo", "forward", lease.Using([]facade.LedgerKey{"demo/vm"}, []facade.LedgerKey{"demo/subnet"}))
	if err != nil {
		t.Fatal(err)
	}
	defer h.release()
	original := h.lease.Holder()
	undo, err := beginRollback(ctx, h, "demo", "forward")
	if err != nil {
		t.Fatal(err)
	}
	if undo != h || undo.lease.Holder() != original {
		t.Fatal("rollback replaced its retained claim")
	}
	if _, bounded := undo.lease.Keys().Keys(); !bounded {
		t.Fatal("rollback expanded to the whole ledger collection")
	}
	leaser := lease.NewLeaser(h.log, nil)
	other, err := leaser.Acquire(ctx, "demo", lease.Of("demo/unrelated"), "other", time.Minute)
	if err != nil {
		t.Fatal("unrelated writer blocked:", err)
	}
	defer other.Release(ctx)
	if _, err := leaser.Acquire(ctx, "demo", lease.Of("demo/vm"), "mutator", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("mutated target unprotected:", err)
	}
	if _, err := leaser.Acquire(ctx, "demo", lease.Using([]facade.LedgerKey{"demo/child"}, []facade.LedgerKey{"demo/vm"}), "adornment", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("new dependent admitted:", err)
	}
	if _, err := leaser.Acquire(ctx, "demo", lease.Of("demo/subnet"), "delete", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("dependency unprotected:", err)
	}
	if h.ctx.Err() != nil {
		t.Fatal("forward protection ended before rollback")
	}
}

func TestRollbackRefusesLostProtection(t *testing.T) {
	ctx := context.Background()
	h, err := hold(ctx, t.TempDir(), "demo", "forward", lease.Of("demo/vm"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.release()
	_, v, found, err := h.log.Get(ctx, "demo/.lease")
	if err != nil || !found {
		t.Fatal(err)
	}
	if err := h.log.Forget(ctx, "demo/.lease", v); err != nil {
		t.Fatal(err)
	}
	if _, err := beginRollback(ctx, h, "demo", "forward"); err == nil || !strings.Contains(err.Error(), "retained locks") {
		t.Fatalf("lost protection accepted: %v", err)
	}
}

func TestRollbackRefusesPendingOutcome(t *testing.T) {
	ctx := context.Background()
	h, err := hold(ctx, t.TempDir(), "demo", "forward", lease.Of("demo/vm"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.release()
	if err := h.log.Begin(ctx, "demo/vm", []byte("uncertain create"), facade.LedgerVersionNone); err != nil {
		t.Fatal(err)
	}
	if _, err := beginRollback(ctx, h, "demo", "forward"); err == nil || !strings.Contains(err.Error(), "unresolved intent") {
		t.Fatalf("pending outcome accepted: %v", err)
	}
	entry, _, found, err := h.log.Get(ctx, "demo/vm")
	if err != nil || !found || entry.Phase() != facade.LedgerPending {
		t.Fatal("uncertainty discarded")
	}
	// Protection remains intact until the transaction returns its outstanding result.
	if _, err := lease.NewLeaser(h.log, nil).Acquire(ctx, "demo", lease.Of("demo/vm"), "other", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("failed rollback prematurely released protection:", err)
	}
}
