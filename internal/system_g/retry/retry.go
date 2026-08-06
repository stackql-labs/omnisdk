// Package retry provides concrete facade.RetryPolicy implementations for recovering from
// potentially-ephemeral failures, built for high parallelism: one policy is shared by every
// concurrent request, staggers retries with jitter (noise) so simultaneous failures de-correlate,
// and governs aggregate retry load through a shared Governor so an outage can't become a storm.
//
// The policy is a composition of three independently switchable algorithms, each an interface:
//   - Classifier — what counts as an ephemeral failure (provider rules);
//   - Backoff    — how long to wait / how to stagger (full jitter, decorrelated jitter, …);
//   - Governor   — the aggregate cross-caller control (rate cap, unlimited, …).
package retry

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Classifier decides whether a failed attempt is transient (worth retrying). Provider-specific
// rules plug in here; keep it pure so the same attempt always classifies the same way.
type Classifier interface {
	Transient(a facade.Attempt) bool
}

// Backoff computes how long to wait before a retry — the staggering algorithm. Switchable so the
// spread strategy (full jitter, decorrelated jitter, constant, …) is a choice, not welded in.
// Called concurrently; keep it safe for that (jitter RNG must be concurrency-safe).
type Backoff interface {
	Delay(a facade.Attempt) time.Duration
}

// Governor bounds AGGREGATE retry load across all concurrent callers, so a dependency outage
// can't be amplified into a retry storm. One Governor backs a RetryPolicy and is shared by every
// in-flight retry; implementations must be safe for concurrent use.
type Governor interface {
	// Reserve claims the next retry slot as of now and returns how long the caller must wait
	// before using it — spacing concurrent retries — or ok=false to refuse the retry outright.
	Reserve(now time.Time) (wait time.Duration, ok bool)
}

// ---- Classifier impls -------------------------------------------------------

type statusClassifier struct{ transient map[int]bool }

// NewStatusClassifier treats a transport error, or any of the given statuses, as transient.
func NewStatusClassifier(statuses ...int) Classifier {
	m := make(map[int]bool, len(statuses))
	for _, s := range statuses {
		m[s] = true
	}
	return statusClassifier{transient: m}
}

func (c statusClassifier) Transient(a facade.Attempt) bool {
	if a.Err != nil {
		return true // a transport error never produced a status — assume transient
	}
	return c.transient[a.Status]
}

// DefaultTransient is the common web-API set: 429 + 502/503/504 (+ transport errors). 500 is
// deliberately excluded — a bare 500 is often a deterministic bug, not backpressure.
func DefaultTransient() Classifier {
	return NewStatusClassifier(429, 502, 503, 504)
}

// ---- Backoff impls ----------------------------------------------------------

func normDur(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// fullJitter: wait ∈ [0, min(cap, base·2^index)] — AWS "full jitter". Maximum de-correlation.
type fullJitter struct{ base, cap time.Duration }

// NewFullJitter builds a full-jitter exponential backoff.
func NewFullJitter(base, cap time.Duration) Backoff {
	return fullJitter{base: normDur(base, 250*time.Millisecond), cap: normDur(cap, 30*time.Second)}
}

func (b fullJitter) Delay(a facade.Attempt) time.Duration {
	ceiling := b.base << a.Index // base · 2^index
	if ceiling <= 0 || ceiling > b.cap {
		ceiling = b.cap // overflow or over cap
	}
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}

// decorrelatedJitter: wait ∈ [base, min(cap, prevWait·3)] — AWS "decorrelated jitter". Spreads
// retries wider over time than full jitter; stateful via Attempt.PrevWait.
type decorrelatedJitter struct{ base, cap time.Duration }

// NewDecorrelatedJitter builds a decorrelated-jitter backoff.
func NewDecorrelatedJitter(base, cap time.Duration) Backoff {
	return decorrelatedJitter{base: normDur(base, 250*time.Millisecond), cap: normDur(cap, 30*time.Second)}
}

func (b decorrelatedJitter) Delay(a facade.Attempt) time.Duration {
	prev := a.PrevWait
	if prev < b.base {
		prev = b.base
	}
	hi := prev * 3
	if hi <= 0 || hi > b.cap {
		hi = b.cap
	}
	if hi <= b.base {
		return b.base
	}
	return b.base + time.Duration(rand.Int64N(int64(hi-b.base)+1)) // [base, hi]
}

// constant: a fixed wait every retry (no exponential growth). Useful for steady polling.
type constant struct{ d time.Duration }

// NewConstant builds a fixed-delay backoff.
func NewConstant(d time.Duration) Backoff { return constant{d: normDur(d, time.Second)} }

func (b constant) Delay(facade.Attempt) time.Duration { return b.d }

// ---- Governor impls ---------------------------------------------------------

// unlimited never refuses and never waits — retries are paced only by the Backoff.
type unlimited struct{}

// NewUnlimited is a Governor that imposes no aggregate cap (backoff-only staggering).
func NewUnlimited() Governor { return unlimited{} }

func (unlimited) Reserve(time.Time) (time.Duration, bool) { return 0, true }

// rateGovernor spaces retries so that, across all callers, they issue at no more than perSecond
// (with a small burst). Concurrent Reserve calls are serialised onto a moving schedule, so N
// simultaneous failures are staggered instead of firing at once.
type rateGovernor struct {
	mu       sync.Mutex
	interval time.Duration // 1/perSecond: min spacing between retries
	burst    time.Duration // how far ahead of now a reservation may still be "free"
	next     time.Time     // earliest instant the next retry may fire
}

// NewRateGovernor caps aggregate retries to perSecond (burst allows a short catch-up).
func NewRateGovernor(perSecond float64, burst int) Governor {
	if perSecond <= 0 {
		return unlimited{}
	}
	interval := time.Duration(float64(time.Second) / perSecond)
	if burst < 1 {
		burst = 1
	}
	return &rateGovernor{interval: interval, burst: time.Duration(burst) * interval}
}

func (g *rateGovernor) Reserve(now time.Time) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.next.Before(now) {
		g.next = now
	}
	at := g.next
	g.next = g.next.Add(g.interval)
	wait := at.Sub(now)
	if wait > g.burst {
		wait = g.burst // cap how long any single caller is asked to hold
	}
	return wait, true
}

// ---- RetryPolicy ------------------------------------------------------------

type policy struct {
	tries   int
	class   Classifier
	backoff Backoff
	gov     Governor
}

// Config parameterises New. Zero values fall back to sensible defaults (full jitter, unlimited
// governor, DefaultTransient, 4 tries).
type Config struct {
	Tries      int        // total attempts incl. the first (default 4)
	Classifier Classifier // what is transient (default DefaultTransient)
	Backoff    Backoff    // stagger algorithm (default NewFullJitter(250ms, 30s))
	Governor   Governor   // aggregate stagger/rate control (default NewUnlimited)
}

// New builds a RetryPolicy from cfg, composing the three switchable algorithms.
func New(cfg Config) facade.RetryPolicy {
	if cfg.Tries <= 0 {
		cfg.Tries = 4
	}
	if cfg.Classifier == nil {
		cfg.Classifier = DefaultTransient()
	}
	if cfg.Backoff == nil {
		cfg.Backoff = NewFullJitter(0, 0)
	}
	if cfg.Governor == nil {
		cfg.Governor = NewUnlimited()
	}
	return &policy{tries: cfg.Tries, class: cfg.Classifier, backoff: cfg.Backoff, gov: cfg.Governor}
}

func (p *policy) Recover(_ context.Context, a facade.Attempt) (time.Duration, bool) {
	if a.Index+1 >= p.tries {
		return 0, false // backstop: out of attempts
	}
	if !p.class.Transient(a) {
		return 0, false // permanent failure
	}
	gwait, ok := p.gov.Reserve(time.Now())
	if !ok {
		return 0, false // aggregate governor refused (budget/circuit)
	}
	if a.RetryAfter > 0 {
		return a.RetryAfter + gwait, true // server hint wins over the backoff algorithm
	}
	return p.backoff.Delay(a) + gwait, true
}

// ---- per-run context carrier ------------------------------------------------

type ctxKey struct{}

// noop is the RetryPolicy in force when none is set: never retries (first failure is terminal).
type noop struct{}

func (noop) Recover(context.Context, facade.Attempt) (time.Duration, bool) { return 0, false }

// WithPolicy returns ctx carrying p as the run's shared retry policy. A nil p is a no-op.
func WithPolicy(ctx context.Context, p facade.RetryPolicy) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, p)
}

// From returns the retry policy on ctx, or a never-retry policy if none is set.
func From(ctx context.Context) facade.RetryPolicy {
	if p, ok := ctx.Value(ctxKey{}).(facade.RetryPolicy); ok && p != nil {
		return p
	}
	return noop{}
}
