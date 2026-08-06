package plan

import (
	"context"
	"errors"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// MergeRows fans several row operators — possibly DISJOINT query graphs — into ONE output cursor.
// This is how System-G owns arbitrary, even disconnected, plans while presenting a client the single
// output node it always sees: `ComposeRows` each subgraph, `MergeRows` them, and the consumer opens
// one Rows and cannot tell whether one graph or a forest produced it. Sources run concurrently; the
// merged stream completes when all do; the first error cancels the rest (via ctx) and is returned.
// Emission order across sources is not defined.
func MergeRows(ops ...facade.Operator) facade.Operator {
	return mergeRows{ops: ops}
}

// MergeComposeRows composes several plans and fans their egress-shaped rows into ONE budget-aware row
// terminal. Each plan keeps its OWN egress (e.g. per-provider blob normalization), applied per-leg
// BEFORE the merge; only the single terminal enforces the run-wide result budget — so --limit caps
// the UNION globally, and the consumer drains one terminal buffer (read off the un-aborted ctx, so a
// limit-abort stops producers without discarding produced rows). This is the multi-cloud audit.
func MergeComposeRows(id int64, plans ...Plan) facade.Operator {
	shaped := make([]facade.Operator, 0, len(plans))
	for i, p := range plans {
		op, err := composePipeline(id+int64((i+1)*1000), p) // disjoint id ranges per leg
		if err != nil {
			return errorOp{err: err}
		}
		shaped = append(shaped, egressOp{upstream: op, transforms: p.Egress()})
	}
	return newRowsOutput(MergeRows(shaped...), nil) // terminal applies the budget only (no egress)
}

// egressOp applies a plan's egress chain per record (drop-aware) with NO budget — the per-leg shaping
// that runs before MergeComposeRows' shared, budget-enforcing terminal.
type egressOp struct {
	upstream   facade.Operator
	transforms []facade.Transform
}

func (e egressOp) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1024, 0)
	in := e.upstream.Open(ctx)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		defer in.Close()
		for in.Next(ctx) {
			rec, drop, err := applyEgress(e.transforms, in.Record())
			if err != nil {
				cerr = err
				return
			}
			if drop {
				continue
			}
			if err := buf.Append(ctx, rec); err != nil {
				if !errors.Is(err, buffer.ErrAllReadersClosed) {
					cerr = err
				}
				return
			}
		}
		cerr = in.Err()
	}()
	return buf.Reader()
}

type mergeRows struct{ ops []facade.Operator }

func (m mergeRows) Open(parent context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1024, 0)
	ctx, cancel := context.WithCancel(parent)
	go func() {
		var (
			mu       sync.Mutex // buffer.Append is single-producer; serialize across sources
			wg       sync.WaitGroup
			errMu    sync.Mutex
			firstErr error
		)
		fail := func(err error) {
			if err == nil {
				return
			}
			errMu.Lock()
			if firstErr == nil {
				firstErr = err
				cancel() // stop the other subgraphs
			}
			errMu.Unlock()
		}
		emit := func(rec facade.Record) error {
			mu.Lock()
			defer mu.Unlock()
			return buf.Append(ctx, rec)
		}
		for _, op := range m.ops {
			wg.Add(1)
			go func(op facade.Operator) {
				defer wg.Done()
				in := op.Open(ctx)
				defer in.Close()
				for in.Next(ctx) {
					if err := emit(in.Record()); err != nil {
						if !errors.Is(err, buffer.ErrAllReadersClosed) {
							fail(err)
						}
						return
					}
				}
				fail(in.Err())
			}(op)
		}
		wg.Wait()
		cancel()
		buf.Complete(firstErr)
	}()
	return buf.Reader()
}
