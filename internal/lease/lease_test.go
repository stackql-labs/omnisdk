package lease_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// clock is a hand-wound clock: expiry is a decision, not a wait.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newLeaser(t *testing.T) (facade.Leaser, *clock) {
	t.Helper()
	l, err := ledger.NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	c := newClock()
	return lease.NewLeaser(l, c.now), c
}

func TestSecondAcquireIsRefusedWhileHeld(t *testing.T) {
	ctx := context.Background()
	l, _ := newLeaser(t)

	if _, err := l.Acquire(ctx, "run", lease.All(), "runner-a", time.Minute); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := l.Acquire(ctx, "run", lease.All(), "runner-b", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Errorf("second acquire = %v, want held", err)
	}
}

// It expires rather than needing a manual unlock: a dead runner frees the collection on its own,
// which is the difference from a lock that requires force-unlock.
func TestExpiredLeaseIsTakenOver(t *testing.T) {
	ctx := context.Background()
	l, c := newLeaser(t)
	if _, err := l.Acquire(ctx, "run", lease.All(), "runner-a", time.Minute); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	c.advance(2 * time.Minute)

	if _, err := l.Acquire(ctx, "run", lease.All(), "runner-b", time.Minute); err != nil {
		t.Errorf("acquire after expiry = %v, want success", err)
	}
}

func TestRenewExtendsExpiry(t *testing.T) {
	ctx := context.Background()
	l, c := newLeaser(t)
	h, err := l.Acquire(ctx, "run", lease.All(), "runner-a", time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	c.advance(30 * time.Second)
	if err := h.Renew(ctx, time.Minute); err != nil {
		t.Fatalf("renew: %v", err)
	}
	c.advance(45 * time.Second)

	if _, err := l.Acquire(ctx, "run", lease.All(), "runner-b", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Errorf("acquire after renew = %v, want still held", err)
	}
}

func TestReleaseFreesTheScope(t *testing.T) {
	ctx := context.Background()
	l, _ := newLeaser(t)
	h, err := l.Acquire(ctx, "run", lease.All(), "runner-a", time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if err := h.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}

	if _, err := l.Acquire(ctx, "run", lease.All(), "runner-b", time.Minute); err != nil {
		t.Errorf("acquire after release = %v, want success", err)
	}
}

// A holder whose lease lapsed and was taken over must not be able to renew or release it — that
// is what stops a resumed process from stamping on its successor.
func TestBrokenHolderCannotRenewOrRelease(t *testing.T) {
	ctx := context.Background()
	l, c := newLeaser(t)
	stale, err := l.Acquire(ctx, "run", lease.All(), "runner-a", time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	c.advance(2 * time.Minute)
	if _, err := l.Acquire(ctx, "run", lease.All(), "runner-b", time.Minute); err != nil {
		t.Fatalf("takeover: %v", err)
	}

	if err := stale.Renew(ctx, time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Errorf("stale renew = %v, want held by another", err)
	}
	if err := stale.Release(ctx); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Errorf("stale release = %v, want held by another", err)
	}
}

func TestScopesAreIndependent(t *testing.T) {
	ctx := context.Background()
	l, _ := newLeaser(t)
	if _, err := l.Acquire(ctx, "run-1", lease.All(), "runner-a", time.Minute); err != nil {
		t.Fatalf("acquire run-1: %v", err)
	}

	if _, err := l.Acquire(ctx, "run-2", lease.All(), "runner-b", time.Minute); err != nil {
		t.Errorf("acquire run-2 = %v, want independent scopes", err)
	}
}

func TestConcurrentAcquireAdmitsExactlyOne(t *testing.T) {
	ctx := context.Background()
	l, _ := newLeaser(t)

	const racers = 16
	var wg sync.WaitGroup
	errs := make([]error, racers)
	wg.Add(racers)
	for i := range racers {
		go func() {
			defer wg.Done()
			_, errs[i] = l.Acquire(ctx, "run", lease.All(), "runner", time.Minute)
		}()
	}
	wg.Wait()

	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		}
	}
	if won != 1 {
		t.Errorf("winners = %d, want exactly 1", won)
	}
}

// The key set is a parameter from day one even though v1 always passes everything, so narrowing
// is configuration rather than a rewrite.
func TestKeySetIntersection(t *testing.T) {
	if !lease.All().Intersects(lease.Of("a")) {
		t.Error("all should intersect any set")
	}
	if !lease.Of("a", "b").Intersects(lease.Of("b", "c")) {
		t.Error("overlapping sets should intersect")
	}
	if lease.Of("a").Intersects(lease.Of("b")) {
		t.Error("disjoint sets should not intersect")
	}
	if !lease.Of("a").Contains("a") || lease.Of("a").Contains("z") {
		t.Error("Contains disagrees with membership")
	}
}

func TestDisjointWritersAndAtomicRelease(t *testing.T) {
	ctx := context.Background()
	l, _ := newLeaser(t)
	a, err := l.Acquire(ctx, "iac", lease.Of("iac/key", "iac/vm"), "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.Acquire(ctx, "iac", lease.Of("iac/subnet"), "b", time.Minute)
	if err != nil {
		t.Fatal("independent writer blocked:", err)
	}
	if _, err := l.Acquire(ctx, "iac", lease.Of("iac/vm", "iac/other"), "c", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatalf("overlap: %v", err)
	}
	// A failed multi-key claim must not reserve its nonconflicting part.
	c, err := l.Acquire(ctx, "iac", lease.Of("iac/other"), "c", time.Minute)
	if err != nil {
		t.Fatal("partial claim leaked:", err)
	}
	if err := lease.Change(ctx, a, lease.Of("iac/vm")); err != nil {
		t.Fatal(err)
	}
	key, err := l.Acquire(ctx, "iac", lease.Of("iac/key"), "next", time.Minute)
	if err != nil {
		t.Fatal("terminal key not released:", err)
	}
	if _, err := l.Acquire(ctx, "iac", lease.Of("iac/vm"), "next", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("vm released prematurely:", err)
	}
	if err := lease.Change(ctx, a, lease.All()); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("compensation overlapped another writer:", err)
	}
	for _, h := range []facade.Lease{a, b, c, key} {
		if err := h.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Acquire(ctx, "iac", lease.All(), "destroy", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredHandleCannotReviveWithoutTakeover(t *testing.T) {
	l, c := newLeaser(t)
	h, err := l.Acquire(context.Background(), "iac", lease.Of("vm"), "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(time.Minute)
	if err := h.Renew(context.Background(), time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatalf("expired holder revived: %v", err)
	}
}

func TestResourceClaimsSurviveReopen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	log, err := ledger.NewFile(root)
	if err != nil {
		t.Fatal(err)
	}
	l := lease.NewLeaser(log, nil)
	h, err := l.Acquire(ctx, "iac", lease.Of("vm"), "first", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := ledger.NewFile(root)
	if err != nil {
		t.Fatal(err)
	}
	next := lease.NewLeaser(reopened, nil)
	if _, err := next.Acquire(ctx, "iac", lease.Of("vm"), "restart", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal(err)
	}
	if _, err := next.Acquire(ctx, "iac", lease.Of("key"), "independent", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := h.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Acquire(ctx, "iac", lease.Of("vm"), "restart", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestSharedSubnetGuardsAllowIndependentAttachments(t *testing.T) {
	ctx := context.Background()
	l, _ := newLeaser(t)
	first, err := l.Acquire(ctx, "iac", lease.Using([]facade.LedgerKey{"vm-a"}, []facade.LedgerKey{"subnet"}), "a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.Acquire(ctx, "iac", lease.Using([]facade.LedgerKey{"vm-b"}, []facade.LedgerKey{"subnet"}), "b", time.Minute)
	if err != nil {
		t.Fatal("independent attachment blocked:", err)
	}
	if _, err := l.Acquire(ctx, "iac", lease.Of("subnet"), "delete", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("subnet deletion admitted during use:", err)
	}
	if _, err := l.Acquire(ctx, "iac", lease.Using([]facade.LedgerKey{"vm-a"}, []facade.LedgerKey{"subnet"}), "duplicate", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("same VM has two writers:", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(ctx, "iac", lease.Of("subnet"), "delete", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("remaining attachment guard lost:", err)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatal(err)
	}
	deletion, err := l.Acquire(ctx, "iac", lease.Of("subnet"), "delete", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer deletion.Release(ctx)
	if _, err := l.Acquire(ctx, "iac", lease.Using([]facade.LedgerKey{"vm-c"}, []facade.LedgerKey{"subnet"}), "c", time.Minute); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Fatal("attachment admitted during subnet write:", err)
	}
}
