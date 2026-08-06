package abort

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// The shared budget must hand out EXACTLY limit slots across concurrent outputs, with exactly one
// "last" — this is what makes --limit N a global cap even when disjoint output DAGs race.
func TestBudgetTakesExactlyLimitUnderConcurrency(t *testing.T) {
	const limit = 100
	b := &Budget{limit: limit}
	var okCount, lastCount int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ { // 16*50 = 800 attempts, well over the limit
				ok, last := b.Take()
				if ok {
					atomic.AddInt64(&okCount, 1)
				}
				if last {
					atomic.AddInt64(&lastCount, 1)
				}
			}
		}()
	}
	wg.Wait()
	if okCount != limit {
		t.Fatalf("granted %d slots, want exactly %d", okCount, limit)
	}
	if lastCount != 1 {
		t.Fatalf("last=true fired %d times, want exactly 1", lastCount)
	}
}

func TestWithLimit(t *testing.T) {
	if BudgetFrom(context.Background()) != nil {
		t.Fatal("no budget expected on a bare ctx")
	}
	if BudgetFrom(WithLimit(context.Background(), 0)) != nil {
		t.Fatal("limit < 1 must carry no budget (unlimited)")
	}
	if BudgetFrom(WithLimit(context.Background(), 3)) == nil {
		t.Fatal("limit 3 must carry a budget")
	}
}

func TestSignalAbortsContext(t *testing.T) {
	ctx, cancel := WithSignal(context.Background())
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("ctx cancelled before Abort")
	default:
	}
	From(ctx).Abort()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Abort did not cancel the ctx")
	}
	From(ctx).Abort()                  // idempotent — no panic
	From(context.Background()).Abort() // no signal → no-op, no panic
}
