package bind

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/schedule"
)

var (
	_ facade.Exchange = &bindJoinExchange{}
)

// InnerFactory produces the dependent inner operator for one outer row, given the bound
// slots copied across the β edges (paper §β: β is identity value-transfer into A^raw).
type InnerFactory func(bound map[string]any) facade.Operator

// Tuple slot keys for a bind-join output record: the outer driver row (Input) paired with one
// inner row (Output, nil under left-outer). The join does NOT merge them — β carries no
// transform (paper §β), so flattening/merging is a downstream T (see NewTupleFlatten).
const (
	KeyInput  = "input"
	KeyOutput = "output"
)

// Presentation tags (paper §A: attribute role/context). A slot may carry KeyTag telling the
// output node how to present it; TagRaw means "log/emit the slot's Raw field verbatim".
const (
	KeyTag = "_tag"
	KeyRaw = "raw"
	TagRaw = "raw"
)

// bindJoinExchange realises the β dataflow of Figure GN-1 as the dependent nested-loop (bind)
// join of paper §W: it drives the outer operator, and per outer row copies the bound slots
// (named by its Bindings' Src→Tgt) into the inner's input, invokes the inner, and emits one
// (input, output) TUPLE per inner row — pairing, not merging. It is a LEFT-OUTER join: an
// outer row whose inner yields nothing still emits (input, null) (paper §W: S(a)=∅ → (a,
// null)). The Bindings are load-bearing — the join reads Src/Tgt to move the values — not
// decoration; there is no planner walking edges, the join is the driver (impl notes:
// "BindJoin lives in the DRIVER").
type bindJoinExchange struct {
	id       int64
	emits    []facade.Beta
	receives []facade.Beta
	readers  int

	outer    facade.Operator
	bindings []Binding
	inner    InnerFactory
	alpha    facade.Alpha // behavioural (E_α) annotation on the outer→inner edge; nil = none
}

// NewBindJoin wires outer ⋈ inner over the given β bindings. alpha is the behavioural (E_α)
// annotation on the outer→inner edge (e.g. a traversal delay); nil for none.
func NewBindJoin(id int64, outer facade.Operator, bindings []Binding, inner InnerFactory, alpha facade.Alpha, readers int) facade.Exchange {
	recv := make([]facade.Beta, len(bindings))
	for i, b := range bindings {
		recv[i] = b // the β edges this join consumes (inspectable topology)
	}
	return &bindJoinExchange{
		id:       id,
		outer:    outer,
		bindings: bindings,
		inner:    inner,
		alpha:    alpha,
		readers:  readers,
		emits:    []facade.Beta{},
		receives: recv,
	}
}

func (e *bindJoinExchange) AddEmit(b facade.Beta)    { e.emits = append(e.emits, b) }
func (e *bindJoinExchange) AddReceive(b facade.Beta) { e.receives = append(e.receives, b) }
func (e *bindJoinExchange) Emits() []facade.Beta     { return e.emits }
func (e *bindJoinExchange) Receives() []facade.Beta  { return e.receives }

func (e *bindJoinExchange) getReaderCount() int {
	if e.readers < 1 {
		return 1
	}
	return e.readers
}

func (e *bindJoinExchange) WriteTo(w io.Writer) (int64, error) {
	var total int64
	n, err := fmt.Fprintf(w, "BindJoin Exchange id = %d ", e.id)
	total += int64(n)
	if err != nil {
		return total, err
	}
	for _, b := range e.bindings {
		m, err := b.WriteTo(w)
		total += m
		if err != nil {
			return total, err
		}
	}
	if e.alpha != nil {
		m, err := e.alpha.WriteTo(w)
		total += m
		if err != nil {
			return total, err
		}
	}
	_, _ = w.Write([]byte("\n"))
	return total + 1, nil
}

// Open fans the inner out across outer rows optimistically and EAGERLY: each row's inner is launched
// as soon as that row arrives — the outer is NOT drained first — so a deep pipeline (descent →
// projects → buckets) streams instead of running stage-by-stage. Each row is an independent unit,
// concurrency gated by the ctx limiter (schedule.LimiterFrom) acquired inside the row (as in
// schedule.Run); rows carry no cross-row dependency, so emission order is not preserved. First error
// cancels in-flight rows and is returned after they drain.
func (e *bindJoinExchange) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(e.getReaderCount(), 1024, 0)
	outer := e.outer.Open(ctx)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		defer outer.Close()

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		lim := schedule.LimiterFrom(ctx)

		var mu sync.Mutex
		emit := func(rec facade.Record) error {
			mu.Lock()
			defer mu.Unlock()
			if err := buf.Append(ctx, rec); err != nil && !errors.Is(err, buffer.ErrAllReadersClosed) {
				return err
			}
			return nil
		}

		var wg sync.WaitGroup
		var errMu sync.Mutex
		var firstErr error
		fail := func(err error) {
			if err == nil {
				return
			}
			errMu.Lock()
			if firstErr == nil {
				firstErr = err
				cancel() // stop in-flight rows; they observe ctx.Done
			}
			errMu.Unlock()
		}

		for outer.Next(ctx) {
			row, ok := DocMap(outer.Record())
			if !ok {
				continue
			}
			bound := make(map[string]any, len(e.bindings))
			for _, b := range e.bindings {
				bound[b.Tgt()] = row[b.Src()]
			}
			n := &rowNode{omap: row, bound: bound, e: e, emit: emit}
			wg.Add(1)
			go func() {
				defer wg.Done()
				fail(runRow(ctx, n, lim))
			}()
		}
		if err := outer.Err(); err != nil {
			fail(err)
		}
		wg.Wait()
		cerr = firstErr
	}()
	return buf.Reader()
}

// runRow acquires a fan-out slot (if a limiter is set), runs the row's inner, and releases it —
// the per-row gate schedule.Run applied, kept for the eager streaming path.
func runRow(ctx context.Context, n *rowNode, lim facade.Limiter) error {
	if lim != nil {
		tok, err := lim.Acquire(ctx)
		if err != nil {
			return err
		}
		defer tok.Release()
	}
	return n.Run(ctx)
}

// rowNode drives the inner for one outer row and emits its (input, output) tuples (left-outer if
// the inner is empty). It first honours the edge's E_α annotation (a traversal delay).
type rowNode struct {
	omap, bound map[string]any
	e           *bindJoinExchange
	emit        func(facade.Record) error
}

func (n *rowNode) Run(ctx context.Context) error {
	if err := n.e.traverseDelay(ctx); err != nil {
		return err
	}
	in := n.e.inner(n.bound).Open(ctx)
	defer in.Close()
	matched := false
	for in.Next(ctx) {
		imap, ok := DocMap(in.Record())
		if !ok {
			continue
		}
		if err := n.emit(NewDocRecord(tuple(n.omap, imap))); err != nil {
			return err
		}
		matched = true
	}
	if err := in.Err(); err != nil {
		return err
	}
	if !matched {
		return n.emit(NewDocRecord(tuple(n.omap, nil)))
	}
	return nil
}

// traverseDelay honours the edge's E_α timing annotation with a ctx-aware wait (0 = no-op).
func (e *bindJoinExchange) traverseDelay(ctx context.Context) error {
	if e.alpha == nil || e.alpha.Delay() <= 0 {
		return nil
	}
	t := time.NewTimer(e.alpha.Delay())
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tuple pairs an outer (input) row with one inner (output) row without merging — the β
// outflow of the join. output is any (not map[string]any) so a left-outer nil stays a true
// nil interface, not a typed-nil map.
func tuple(input map[string]any, output any) map[string]any {
	return map[string]any{KeyInput: input, KeyOutput: output}
}

// mergeMaps returns a ∪ b; b wins on key conflict. Used by NewTupleFlatten, not the join.
func mergeMaps(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
