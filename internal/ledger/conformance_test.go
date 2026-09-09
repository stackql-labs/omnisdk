package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// each runs a case against every implementation. The semantics are the contract, not the storage:
// a durable Ledger that diverges from the in-memory reference is wrong, so both are held to one
// suite rather than tested apart.
func each(t *testing.T, body func(*testing.T, facade.Ledger)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { body(t, ledger.NewMemory()) })
	t.Run("file", func(t *testing.T) {
		l, err := ledger.NewFile(t.TempDir())
		if err != nil {
			t.Fatalf("new file ledger: %v", err)
		}
		body(t, l)
	})
}

func get(t *testing.T, l facade.Ledger, k facade.LedgerKey) (facade.LedgerEntry, facade.LedgerVersion) {
	t.Helper()
	e, v, ok, err := l.Get(context.Background(), k)
	if err != nil {
		t.Fatalf("get %s: %v", k, err)
	}
	if !ok {
		t.Fatalf("get %s: absent", k)
	}
	return e, v
}

func TestBeginRecordsIntentWithoutPlan(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()

		if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
			t.Fatalf("begin: %v", err)
		}

		e, _ := get(t, l, "s/a")
		if e.Phase() != facade.LedgerPending {
			t.Errorf("phase = %v, want pending", e.Phase())
		}
		if string(e.Proposed()) != "n" {
			t.Errorf("proposed = %q, want n", e.Proposed())
		}
		if e.Plan() != nil {
			t.Errorf("plan = %q, want nil before any resolve", e.Plan())
		}
	})
}

func TestResolvePromotesProposedToPlan(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()
		if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, v := get(t, l, "s/a")

		if err := l.Resolve(ctx, "s/a", []byte("id-1"), v); err != nil {
			t.Fatalf("resolve: %v", err)
		}

		e, _ := get(t, l, "s/a")
		if e.Phase() != facade.LedgerLive {
			t.Errorf("phase = %v, want live", e.Phase())
		}
		if string(e.Plan()) != "n" {
			t.Errorf("plan = %q, want n", e.Plan())
		}
		if e.Proposed() != nil {
			t.Errorf("proposed = %q, want nil once live", e.Proposed())
		}
		if string(e.Identity()) != "id-1" {
			t.Errorf("identity = %q, want id-1", e.Identity())
		}
	})
}

// The n-1 invariant: an update in flight must leave the previous resolved intent readable, or a
// failed call loses the basis for a three-way merge and a dropped field can never be unset.
func TestBeginNeverOverwritesPlan(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()
		if err := l.Begin(ctx, "s/a", []byte("n-1"), facade.LedgerVersionNone); err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, v := get(t, l, "s/a")
		if err := l.Resolve(ctx, "s/a", []byte("id-1"), v); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		_, v = get(t, l, "s/a")

		if err := l.Begin(ctx, "s/a", []byte("n"), v); err != nil {
			t.Fatalf("begin update: %v", err)
		}

		e, _ := get(t, l, "s/a")
		if string(e.Plan()) != "n-1" {
			t.Errorf("plan = %q, want n-1 preserved across an in-flight update", e.Plan())
		}
		if string(e.Proposed()) != "n" {
			t.Errorf("proposed = %q, want n", e.Proposed())
		}
	})
}

func TestStaleVersionConflicts(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()
		if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, stale := get(t, l, "s/a")
		if err := l.Resolve(ctx, "s/a", []byte("id-1"), stale); err != nil {
			t.Fatalf("resolve: %v", err)
		}

		if err := l.Begin(ctx, "s/a", []byte("other"), stale); !errors.Is(err, facade.ErrLedgerConflict) {
			t.Errorf("begin on stale version = %v, want conflict", err)
		}
	})
}

func TestCreateOnExistingKeyConflicts(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()
		if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
			t.Fatalf("begin: %v", err)
		}

		if err := l.Begin(ctx, "s/a", []byte("other"), facade.LedgerVersionNone); !errors.Is(err, facade.ErrLedgerConflict) {
			t.Errorf("create over existing = %v, want conflict", err)
		}
	})
}

// A retried run presents the same proposal; it must not be told it conflicts with itself.
func TestBeginIsIdempotentForAnIdenticalProposal(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()
		if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
			t.Fatalf("begin: %v", err)
		}

		if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
			t.Errorf("repeat begin = %v, want no-op", err)
		}
	})
}

func TestResolveRequiresPending(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()
		if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, v := get(t, l, "s/a")
		if err := l.Resolve(ctx, "s/a", []byte("id-1"), v); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		_, v = get(t, l, "s/a")

		if err := l.Resolve(ctx, "s/a", []byte("id-2"), v); !errors.Is(err, facade.ErrLedgerPhase) {
			t.Errorf("resolve of a live entry = %v, want phase error", err)
		}
	})
}

func TestListIsScopedAndOrdered(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()
		for _, k := range []facade.LedgerKey{"run-2/b", "run-1/b", "run-1/a"} {
			if err := l.Begin(ctx, k, []byte("n"), facade.LedgerVersionNone); err != nil {
				t.Fatalf("begin %s: %v", k, err)
			}
		}

		got, err := l.List(ctx, "run-1/")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2 entries under run-1/", len(got))
		}
		if got[0].Key() != "run-1/a" || got[1].Key() != "run-1/b" {
			t.Errorf("keys = %s,%s, want run-1/a,run-1/b", got[0].Key(), got[1].Key())
		}
	})
}

func TestForgetRemovesTheEntry(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()
		if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, v := get(t, l, "s/a")

		if err := l.Forget(ctx, "s/a", v); err != nil {
			t.Fatalf("forget: %v", err)
		}

		if _, _, ok, err := l.Get(ctx, "s/a"); err != nil || ok {
			t.Errorf("get after forget = ok:%v err:%v, want absent", ok, err)
		}
	})
}

// Exactly one creator wins the key; every other must be told to re-read. This is the durable
// mutex the design leans on, so it is asserted under real contention rather than by inspection.
func TestConcurrentCreateAdmitsExactlyOne(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()

		const racers = 32
		var wg sync.WaitGroup
		errs := make([]error, racers)
		wg.Add(racers)
		for i := range racers {
			go func() {
				defer wg.Done()
				errs[i] = l.Begin(ctx, "s/a", fmt.Appendf(nil, "proposal-%d", i), facade.LedgerVersionNone)
			}()
		}
		wg.Wait()

		won := 0
		for i, err := range errs {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, facade.ErrLedgerConflict):
				t.Errorf("racer %d = %v, want nil or conflict", i, err)
			}
		}
		if won != 1 {
			t.Errorf("winners = %d, want exactly 1", won)
		}
	})
}

// Entries handed out must not alias the store, or a caller can corrupt n-1 by holding a slice.
func TestReturnedEntriesDoNotAliasTheStore(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()
		if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
			t.Fatalf("begin: %v", err)
		}

		e, _ := get(t, l, "s/a")
		e.Proposed()[0] = 'X'

		again, _ := get(t, l, "s/a")
		if string(again.Proposed()) != "n" {
			t.Errorf("proposed = %q after caller mutation, want n", again.Proposed())
		}
	})
}

// Contention on an existing key, not merely on creation: every writer holds the same version and
// exactly one may land. This is the compare half of compare-and-swap, and on disk it is what the
// link does — rename cannot do it.
func TestConcurrentUpdateAdmitsExactlyOne(t *testing.T) {
	each(t, func(t *testing.T, l facade.Ledger) {
		ctx := context.Background()
		if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, v := get(t, l, "s/a")
		if err := l.Resolve(ctx, "s/a", []byte("id-1"), v); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		_, shared := get(t, l, "s/a")

		const racers = 16
		var wg sync.WaitGroup
		errs := make([]error, racers)
		wg.Add(racers)
		for i := range racers {
			go func() {
				defer wg.Done()
				errs[i] = l.Begin(ctx, "s/a", fmt.Appendf(nil, "proposal-%d", i), shared)
			}()
		}
		wg.Wait()

		won := 0
		for i, err := range errs {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, facade.ErrLedgerConflict):
				t.Errorf("racer %d = %v, want nil or conflict", i, err)
			}
		}
		if won != 1 {
			t.Errorf("winners = %d, want exactly 1", won)
		}
	})
}
