// Package admit provides admission control — concurrency limiting keyed by shared-resource scope.
// It answers "how optimistic to be with concurrency": unlimited (schedule everything), a fixed
// semaphore ("N at a time"), or an adaptive limit. The keying is the crux — everything hitting one
// backend (e.g. an AWS account) contends on one Limiter, while work on unrelated scopes runs free,
// so independent nodes across independent graphs throttle together exactly when they share a resource.
//
// Concrete implementations of the facade.Limiter / facade.Admissions interfaces live here; the
// active Admissions is carried per-run on the context.
package admit

import (
	"context"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// ---- Limiter impls ----------------------------------------------------------

// semaphore admits at most n concurrent holders. Acquire blocks (honouring ctx) until a slot is
// free; Release frees one. A buffered channel is the counting semaphore.
type semaphore struct{ slots chan struct{} }

// NewSemaphore is a fixed "N at a time" limiter (n<1 becomes 1).
func NewSemaphore(n int) facade.Limiter {
	if n < 1 {
		n = 1
	}
	return semaphore{slots: make(chan struct{}, n)}
}

func (s semaphore) Acquire(ctx context.Context) (facade.Token, error) {
	select {
	case s.slots <- struct{}{}:
		return &relToken{slots: s.slots}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// relToken frees one semaphore slot on Release; once guards against a double release draining two.
type relToken struct {
	slots chan struct{}
	once  sync.Once
}

func (t *relToken) Release() { t.once.Do(func() { <-t.slots }) }

// unlimited never blocks — full optimism (schedule everything).
type unlimited struct{}

// NewUnlimited is a limiter that imposes no cap.
func NewUnlimited() facade.Limiter { return unlimited{} }

func (unlimited) Acquire(context.Context) (facade.Token, error) { return noopToken{}, nil }

type noopToken struct{}

func (noopToken) Release() {}

// ---- Admissions (keyed registry) --------------------------------------------

// keyed lazily builds and memoises one Limiter per scope key via a factory, so all callers with
// the same key share a Limiter. Safe for concurrent use.
type keyed struct {
	mu       sync.Mutex
	limiters map[string]facade.Limiter
	factory  func(key string) facade.Limiter
}

// NewAdmissions builds a keyed registry: each distinct key gets one Limiter from factory, reused
// for every subsequent lookup of that key.
func NewAdmissions(factory func(key string) facade.Limiter) facade.Admissions {
	return &keyed{limiters: map[string]facade.Limiter{}, factory: factory}
}

// PerScope gives every scope key its own fixed semaphore of size n — "at most n at a time per
// backend".
func PerScope(n int) facade.Admissions {
	return NewAdmissions(func(string) facade.Limiter { return NewSemaphore(n) })
}

func (k *keyed) For(key string) facade.Limiter {
	k.mu.Lock()
	defer k.mu.Unlock()
	l, ok := k.limiters[key]
	if !ok {
		l = k.factory(key)
		k.limiters[key] = l
	}
	return l
}

// open is the default Admissions: every scope is unlimited.
type open struct{}

func (open) For(string) facade.Limiter { return unlimited{} }

// ---- per-run context carrier ------------------------------------------------

type ctxKey struct{}

// WithAdmissions returns ctx carrying a as the run's admission control. A nil a is a no-op.
func WithAdmissions(ctx context.Context, a facade.Admissions) context.Context {
	if a == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, a)
}

// From returns the Admissions on ctx, or an open (unlimited) one if none is set.
func From(ctx context.Context) facade.Admissions {
	if a, ok := ctx.Value(ctxKey{}).(facade.Admissions); ok && a != nil {
		return a
	}
	return open{}
}
