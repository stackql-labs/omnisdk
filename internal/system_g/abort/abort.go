// Package abort makes early, CLEAN termination of a running query a first-class, reachable
// operation. A query already tears down on context cancellation (every operator honours ctx.Done,
// buffers Complete, sinks flush); what was missing is a way to TRIGGER that from inside the run.
//
// Begin-with model: the OUTPUT node triggers the abort. Cancellation then propagates two ways —
// up the connected dependency chain in reverse order (each consumer closing its reader stops its
// producer, which closes its own upstream…), and, as a backstop, to any DISCONNECTED nodes (e.g.
// the three disjoint sub-DAGs of the multi-provider audit) via the context, which cancels them
// wherever they are. In theory any node could trigger, propagating in any order; that generalises
// later. The result budget (--limit N) is the first trigger: a run-wide counter the outputs share,
// so N is a GLOBAL cap even across disconnected outputs.
package abort

import (
	"context"
	"sync/atomic"
)

// Signal terminates the query it is attached to, cleanly: Abort cancels the run context, so every
// operator observing ctx.Done unwinds, buffers Complete, and sinks flush. Safe from any goroutine,
// any number of times (idempotent).
type Signal interface {
	Abort()
}

type (
	key       struct{}
	budgetKey struct{}
)

type cancelSignal struct{ cancel context.CancelFunc }

func (s cancelSignal) Abort() { s.cancel() }

type noop struct{}

func (noop) Abort() {}

// WithSignal derives a cancelable context carrying an abort Signal. The caller defers the returned
// cancel as usual; anything the context reaches can call abort.From(ctx).Abort() to stop the query
// early, with the same clean teardown cancellation already provides.
func WithSignal(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	return context.WithValue(ctx, key{}, Signal(cancelSignal{cancel: cancel})), cancel
}

// From returns the abort Signal carried on ctx, or a no-op if none (so callers never nil-check).
func From(ctx context.Context) Signal {
	if s, ok := ctx.Value(key{}).(Signal); ok {
		return s
	}
	return noop{}
}

// Budget is a run-wide result counter shared by every output — including disconnected sub-DAGs
// (e.g. the multi-provider audit's three trees). It makes --limit N a GLOBAL cap: outputs reserve
// slots atomically, so exactly N records are emitted across the whole query however many disjoint
// outputs run.
type Budget struct {
	limit int64
	count int64
}

// Take reserves the next output slot. ok=false means the budget is already spent (do not emit this
// record — abort). last=true means this call took the FINAL slot (emit it, then abort).
func (b *Budget) Take() (ok, last bool) {
	n := atomic.AddInt64(&b.count, 1)
	return n <= b.limit, n == b.limit
}

// WithLimit carries a run-wide result budget of n (n<1 = unlimited, no budget).
func WithLimit(ctx context.Context, n int) context.Context {
	if n < 1 {
		return ctx
	}
	return context.WithValue(ctx, budgetKey{}, &Budget{limit: int64(n)})
}

// BudgetFrom returns the shared result budget on ctx, or nil (unlimited).
func BudgetFrom(ctx context.Context) *Budget {
	if b, ok := ctx.Value(budgetKey{}).(*Budget); ok {
		return b
	}
	return nil
}
