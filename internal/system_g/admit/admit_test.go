package admit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A semaphore of N must never let more than N holders run at once.
func TestSemaphoreCapsConcurrency(t *testing.T) {
	const n = 3
	lim := NewSemaphore(n)
	var cur, max int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := lim.Acquire(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer tok.Release()
			c := atomic.AddInt32(&cur, 1)
			for {
				m := atomic.LoadInt32(&max)
				if c <= m || atomic.CompareAndSwapInt32(&max, m, c) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt32(&cur, -1)
		}()
	}
	wg.Wait()
	if max > n {
		t.Errorf("observed %d concurrent holders, want <= %d", max, n)
	}
	if max == 0 {
		t.Error("no concurrency observed")
	}
}

// Acquire honours ctx cancellation when no slot is free.
func TestSemaphoreAcquireCtxCancel(t *testing.T) {
	lim := NewSemaphore(1)
	tok, err := lim.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lim.Acquire(ctx); err == nil {
		t.Error("acquire on a full semaphore with a cancelled ctx should error")
	}
	tok.Release()
}

// A double Release must not free an extra slot (the once guard).
func TestTokenDoubleReleaseSafe(t *testing.T) {
	lim := NewSemaphore(1)
	tok, _ := lim.Acquire(context.Background())
	tok.Release()
	tok.Release() // no-op, must not free a phantom slot
	// If the double release leaked a slot, this would let 2 acquire; capacity is 1, so the second
	// must block — verify by a cancelled-ctx acquire after taking the one real slot.
	held, _ := lim.Acquire(context.Background())
	defer held.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lim.Acquire(ctx); err == nil {
		t.Error("double release leaked a slot")
	}
}

// The keyed registry returns the same Limiter per key and distinct Limiters per distinct key.
func TestAdmissionsKeyed(t *testing.T) {
	a := PerScope(2)
	x1, x2 := a.For("aws:acct-a"), a.For("aws:acct-a")
	y := a.For("aws:acct-b")
	if x1 != x2 {
		t.Error("same key must return the same limiter")
	}
	if x1 == y {
		t.Error("distinct keys must return distinct limiters")
	}
}

// The default (no admissions) is open: Acquire never blocks.
func TestOpenDefault(t *testing.T) {
	lim := From(context.Background()).For("anything")
	for i := 0; i < 100; i++ {
		if _, err := lim.Acquire(context.Background()); err != nil {
			t.Fatalf("open limiter should never fail: %v", err)
		}
		// deliberately never released — open never blocks
	}
}
