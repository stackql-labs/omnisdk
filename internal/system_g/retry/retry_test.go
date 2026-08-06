package retry

import (
	"context"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func TestDefaultTransientClassification(t *testing.T) {
	c := DefaultTransient()
	cases := []struct {
		a    facade.Attempt
		want bool
	}{
		{facade.Attempt{Status: 503}, true},
		{facade.Attempt{Status: 429}, true},
		{facade.Attempt{Status: 500}, false}, // bare 500 = likely a bug, not backpressure
		{facade.Attempt{Status: 404}, false},
		{facade.Attempt{Status: 200}, false},
		{facade.Attempt{Err: context.DeadlineExceeded}, true}, // transport error
	}
	for _, tc := range cases {
		if got := c.Transient(tc.a); got != tc.want {
			t.Errorf("Transient(%+v) = %v, want %v", tc.a, got, tc.want)
		}
	}
}

func TestExponentialStopsAtTries(t *testing.T) {
	p := New(Config{Tries: 3, Backoff: NewFullJitter(time.Millisecond, time.Second)})
	// Index 0,1 → retry; Index 2 is the 3rd try → give up.
	if _, ok := p.Recover(context.Background(), facade.Attempt{Index: 0, Status: 503}); !ok {
		t.Error("attempt 0 should retry")
	}
	if _, ok := p.Recover(context.Background(), facade.Attempt{Index: 1, Status: 503}); !ok {
		t.Error("attempt 1 should retry")
	}
	if _, ok := p.Recover(context.Background(), facade.Attempt{Index: 2, Status: 503}); ok {
		t.Error("attempt 2 exhausts tries; must not retry")
	}
}

func TestExponentialPermanentNoRetry(t *testing.T) {
	p := New(Config{Tries: 5})
	if _, ok := p.Recover(context.Background(), facade.Attempt{Index: 0, Status: 400}); ok {
		t.Error("400 is permanent; must not retry")
	}
}

func TestRetryAfterOverridesBackoff(t *testing.T) {
	p := New(Config{Tries: 5, Backoff: NewFullJitter(time.Millisecond, time.Minute)})
	wait, ok := p.Recover(context.Background(), facade.Attempt{Index: 0, Status: 503, RetryAfter: 2 * time.Second})
	if !ok {
		t.Fatal("should retry")
	}
	if wait != 2*time.Second {
		t.Errorf("wait = %v, want the 2s Retry-After hint", wait)
	}
}

func TestFullJitterWithinBounds(t *testing.T) {
	base := 100 * time.Millisecond
	p := New(Config{Tries: 100, Backoff: NewFullJitter(base, time.Hour)})
	ceiling := base << 3 // index 3 → base·2^3 = 800ms
	var sawNonZero bool
	for i := 0; i < 200; i++ {
		wait, ok := p.Recover(context.Background(), facade.Attempt{Index: 3, Status: 503})
		if !ok {
			t.Fatal("should retry")
		}
		if wait < 0 || wait > ceiling {
			t.Fatalf("wait %v outside [0, %v]", wait, ceiling)
		}
		if wait > 0 {
			sawNonZero = true
		}
	}
	if !sawNonZero {
		t.Error("full jitter produced only zero waits — no noise")
	}
}

// Decorrelated jitter stays within [base, cap] and, given a larger PrevWait, can spread further —
// the switchable stagger algorithm the parallel case wants.
func TestDecorrelatedJitterWithinBounds(t *testing.T) {
	base, cap := 50*time.Millisecond, 5*time.Second
	p := New(Config{Tries: 100, Backoff: NewDecorrelatedJitter(base, cap)})
	for i := 0; i < 200; i++ {
		wait, ok := p.Recover(context.Background(), facade.Attempt{Index: 2, Status: 503, PrevWait: 400 * time.Millisecond})
		if !ok {
			t.Fatal("should retry")
		}
		if wait < base || wait > cap {
			t.Fatalf("wait %v outside [%v, %v]", wait, base, cap)
		}
	}
}

func TestRateGovernorSpacesRetries(t *testing.T) {
	g := NewRateGovernor(10, 2) // interval 100ms, burst 200ms
	now := time.Unix(0, 0)
	// Same instant, four reservations: staggered by interval, capped at the burst window.
	got := []time.Duration{}
	for i := 0; i < 4; i++ {
		w, ok := g.Reserve(now)
		if !ok {
			t.Fatal("rate governor should admit")
		}
		got = append(got, w)
	}
	want := []time.Duration{0, 100 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reservation %d wait = %v, want %v (got %v)", i, got[i], want[i], got)
		}
	}
}

func TestUnlimitedGovernor(t *testing.T) {
	g := NewUnlimited()
	if w, ok := g.Reserve(time.Now()); w != 0 || !ok {
		t.Errorf("unlimited = (%v,%v), want (0,true)", w, ok)
	}
}

func TestContextCarrier(t *testing.T) {
	if _, ok := From(context.Background()).Recover(context.Background(), facade.Attempt{Status: 503}); ok {
		t.Error("no policy set → must not retry")
	}
	p := New(Config{Tries: 3})
	ctx := WithPolicy(context.Background(), p)
	if _, ok := From(ctx).Recover(context.Background(), facade.Attempt{Index: 0, Status: 503}); !ok {
		t.Error("policy on ctx must be recovered and retry")
	}
}

// The governor must be safe for concurrent use (run with -race) and admit every caller.
func TestRateGovernorConcurrent(t *testing.T) {
	g := NewRateGovernor(1000, 8)
	now := time.Unix(0, 0)
	const n = 200
	done := make(chan bool, n)
	for i := 0; i < n; i++ {
		go func() {
			_, ok := g.Reserve(now)
			done <- ok
		}()
	}
	for i := 0; i < n; i++ {
		if !<-done {
			t.Fatal("every concurrent reservation should be admitted")
		}
	}
}
