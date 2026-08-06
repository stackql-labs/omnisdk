// Package saga records undo/redo intent for mutating exchanges — the forward action and its
// compensation — so a run's effects can later be rolled back or replayed. It only LOGS intent;
// executing the compensation/replay is future work. Concrete facade.SagaLog implementations live
// here and the active log is carried per-run on the context.
package saga

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// writer is a JSONL facade.SagaLog: one entry per line to an io.Writer, guarded for concurrent
// use (many mutating exchanges may commit in parallel).
type writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter records saga entries as JSONL to w.
func NewWriter(w io.Writer) facade.SagaLog { return &writer{w: w} }

func (l *writer) Record(e facade.SagaEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(append(b, '\n'))
}

// discard drops entries — the default when no saga log is set.
type discard struct{}

// Discard is a no-op saga log.
func Discard() facade.SagaLog { return discard{} }

func (discard) Record(facade.SagaEntry) {}

// ---- per-run context carrier ------------------------------------------------

type ctxKey struct{}

// WithLog returns ctx carrying l as the run's saga log. A nil l is a no-op.
func WithLog(ctx context.Context, l facade.SagaLog) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, l)
}

// From returns the saga log on ctx, or a discarding one if none is set.
func From(ctx context.Context) facade.SagaLog {
	if l, ok := ctx.Value(ctxKey{}).(facade.SagaLog); ok && l != nil {
		return l
	}
	return discard{}
}
