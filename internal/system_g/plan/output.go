package plan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/stackql-labs/omnisdk/internal/system_g/abort"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

var (
	_ facade.Exchange = &outputExchange{}
)

// applyEgress runs the egress transform chain over rec; drop is true if a transform filtered it out
// (returned a nil record). Each transform reads the running record as a Page and returns the next.
func applyEgress(transforms []facade.Transform, rec facade.Record) (facade.Record, bool, error) {
	for _, tr := range transforms {
		out, err := tr.Apply(rec)
		if err != nil {
			return nil, false, err
		}
		if out == nil {
			return nil, true, nil
		}
		rec = out
	}
	return rec, false, nil
}

// drainEgress is the shared terminal loop: pull each upstream record, apply egress, enforce the
// run-wide result budget (aborting the query when spent), and hand survivors to sink. Byte output
// (encode to a writer) and row output (emit to a cursor) differ ONLY in sink. Returns the first
// sink or upstream error.
func drainEgress(ctx context.Context, in facade.Records, transforms []facade.Transform, sink func(facade.Record) error) error {
	budget := abort.BudgetFrom(ctx) // run-wide result cap (nil = unlimited)
	for in.Next(ctx) {
		rec, drop, err := applyEgress(transforms, in.Record())
		if err != nil {
			return err
		}
		if drop {
			continue
		}
		if budget != nil {
			ok, last := budget.Take()
			if !ok {
				// budget already spent by another output — stop, emit nothing more.
				abort.From(ctx).Abort()
				return nil
			}
			if err := sink(rec); err != nil {
				return err
			}
			if last {
				// this output took the final slot: terminate the whole query cleanly.
				abort.From(ctx).Abort()
				return nil
			}
			continue
		}
		if err := sink(rec); err != nil {
			return err
		}
	}
	return in.Err()
}

// outputExchange is the terminal BYTE sink: it applies the egress chain to each upstream record and
// encodes the result to a writer, emitting nothing downstream. Ranging its (empty) cursor to EOF
// drives the pull. It is the row terminal (below) plus an encode sink — one shared loop.
type outputExchange struct {
	id        int64
	emits     []facade.Beta
	receives  []facade.Beta
	formClass facade.FormClass

	upstream   facade.Operator
	transforms []facade.Transform
	encoder    facade.Encoder
	w          io.Writer
}

// NewOutputExchange wires a byte sink that runs transforms (may be nil) over each upstream record on
// edge, then encodes to w (nil w defaults to stdout). Traffic logging is per-exchange via the trace
// sink on the context, not here.
func NewOutputExchange(id int64, upstream facade.Operator, transforms []facade.Transform, encoder facade.Encoder, w io.Writer) facade.Exchange {
	if w == nil {
		w = os.Stdout
	}
	return &outputExchange{
		id:         id,
		upstream:   upstream,
		transforms: transforms,
		encoder:    encoder,
		w:          w,
		formClass:  facade.FormRead,
		emits:      []facade.Beta{},
		receives:   []facade.Beta{},
	}
}

func (e *outputExchange) AddEmit(b facade.Beta)    { e.emits = append(e.emits, b) }
func (e *outputExchange) AddReceive(b facade.Beta) { e.receives = append(e.receives, b) }
func (e *outputExchange) Emits() []facade.Beta     { return e.emits }
func (e *outputExchange) Receives() []facade.Beta  { return e.receives }

func (e *outputExchange) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write([]byte(fmt.Sprintf("Output Exchange id = %d\n", e.id)))
	return int64(n), err
}

func (e *outputExchange) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1024, 0)
	in := e.upstream.Open(ctx)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		defer in.Close()
		cerr = drainEgress(ctx, in, e.transforms, func(rec facade.Record) error {
			_, err := e.encoder.Encode(e.w, rec)
			return err
		})
	}()
	return buf.Reader()
}

// rowsOutput is the terminal ROW sink: same egress + budget loop as the byte terminal, but it EMITS
// each shaped record for a library consumer to iterate (see plan.ComposeRows), rather than encoding.
type rowsOutput struct {
	upstream   facade.Operator
	transforms []facade.Transform
}

func newRowsOutput(upstream facade.Operator, transforms []facade.Transform) facade.Operator {
	return &rowsOutput{upstream: upstream, transforms: transforms}
}

func (e *rowsOutput) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1024, 0)
	in := e.upstream.Open(ctx)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		defer in.Close()
		err := drainEgress(ctx, in, e.transforms, func(rec facade.Record) error {
			return buf.Append(ctx, rec)
		})
		if err != nil && !errors.Is(err, buffer.ErrAllReadersClosed) {
			cerr = err // consumer closing the cursor is a clean stop, not an error
		}
	}()
	return buf.Reader()
}
