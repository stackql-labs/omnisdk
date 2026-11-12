package omnisdk

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func TestResourceLocksReleaseAtLastConsumer(t *testing.T) {
	ctx := context.Background()
	g, err := NewGraph([]Node{NewMutationNode("key", "widgets", "insert", nil, nil, nil), NewMutationNode("vm", "widgets", "insert", nil, nil, nil), NewNode("verify", "widgets", nil)}, []Wiring{
		NewWiring("vm", []Inbound{NewInbound("key", "id", "key")}, "", ""),
		NewWiring("verify", []Inbound{NewInbound("vm", "id", "id")}, "", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]Managed{"key": NewManaged("key", []string{"key"}, "id", "id"), "vm": NewManaged("vm", []string{"vm"}, "id", "id")}
	locks := graphLocks(g, owners, "demo")
	root := t.TempDir()
	h, err := hold(ctx, root, "demo", "run", lease.Of(locks.keys()...))
	if err != nil {
		t.Fatal(err)
	}
	defer h.release()
	locks.h = h
	log, err := ledger.NewFile(filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	challenger := lease.NewLeaser(log, nil)
	if err := locks.complete(ctx, "key"); err != nil {
		t.Fatal(err)
	}
	if _, err := challenger.Acquire(ctx, "demo", lease.Of("demo/key"), "other", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("key released before consumer:", err)
	}
	if err := locks.complete(ctx, "vm"); err != nil {
		t.Fatal(err)
	}
	key, err := challenger.Acquire(ctx, "demo", lease.Of("demo/key"), "key-writer", time.Minute)
	if err != nil {
		t.Fatal("key retained after its last consumer:", err)
	}
	defer key.Release(ctx)
	if _, err := challenger.Acquire(ctx, "demo", lease.Of("demo/vm"), "other", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("vm released before verification:", err)
	}
	if err := locks.complete(ctx, "verify"); err != nil {
		t.Fatal(err)
	}
	other, err := challenger.Acquire(ctx, "demo", lease.Of("demo/vm"), "other", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release(ctx)
}

func TestManagedWritersSerializeWithinRun(t *testing.T) {
	run := &managedRun{}
	release, err := run.lockWriter(context.Background(), "vm")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := run.lockWriter(ctx, "vm"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("overlapping writer admitted:", err)
	}
	independent, err := run.lockWriter(context.Background(), "key")
	if err != nil {
		t.Fatal(err)
	}
	independent()
	release()
	release() // Close after exhaustion is harmless.
	next, err := run.lockWriter(context.Background(), "vm")
	if err != nil {
		t.Fatal(err)
	}
	next()
}

func TestLeaseRenewalAndLossCancelsRun(t *testing.T) {
	ctx := context.Background()
	log := ledger.NewMemory()
	leaser := lease.NewLeaser(log, nil)
	claim, err := leaser.Acquire(ctx, "demo", lease.Of("vm"), "run", 90*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	h := &held{lease: claim}
	h.maintain(ctx, 90*time.Millisecond)
	defer h.release()
	// Several renewal periods must pass without allowing another writer.
	time.Sleep(180 * time.Millisecond)
	if _, err := leaser.Acquire(ctx, "demo", lease.Of("vm"), "other", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("heartbeat failed:", err)
	}
	// Simulate store-level loss of the claim. The next renewal must cancel work.
	_, v, found, err := log.Get(ctx, "demo/.lease")
	if err != nil || !found {
		t.Fatal(err)
	}
	if err := log.Forget(ctx, "demo/.lease", v); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("lost lease did not cancel run")
	}
}
