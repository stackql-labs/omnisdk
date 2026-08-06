package exchange

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

var (
	_ facade.Exchange = &transformExchange{}
)

// transformExchange is a compute node: no I/O side effect (FormRead). It pulls records
// from a single upstream operator, applies an injected Transform per record, and emits
// the results downstream. Streaming and 1→{0,1}: the transform may drop a record
// (filter) by returning a nil record, or map it. Blocking transforms (aggregate/sort)
// are a separate impl — this one keeps bounded memory.
type transformExchange struct {
	id        int64
	emits     []facade.Beta
	receives  []facade.Beta
	upstream  facade.Operator  // producer feeding this node
	transform facade.Transform // the compute applied per input page
	formClass facade.FormClass // FormRead: pure compute, no side effect
	readers   int              // out-degree (buffer reader count)
}

// NewTransformExchange wires a compute node. `transform` is the injected strategy — the
// node applies whatever Transform it is given; there is no fixed set. `readers` is the
// out-degree (how many downstream consumers pull from it).
func NewTransformExchange(
	id int64,
	upstream facade.Operator,
	transform facade.Transform,
	readers int,
) facade.Exchange {
	return &transformExchange{
		id:        id,
		upstream:  upstream,
		transform: transform,
		formClass: facade.FormRead,
		readers:   readers,
		emits:     []facade.Beta{},
		receives:  []facade.Beta{},
	}
}

func (e *transformExchange) AddEmit(b facade.Beta)    { e.emits = append(e.emits, b) }
func (e *transformExchange) AddReceive(b facade.Beta) { e.receives = append(e.receives, b) }
func (e *transformExchange) Emits() []facade.Beta     { return e.emits }
func (e *transformExchange) Receives() []facade.Beta  { return e.receives }

func (e *transformExchange) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write([]byte(fmt.Sprintf("Transform Exchange id = %d\n", e.id)))
	return int64(n), err
}

func (e *transformExchange) Open(ctx context.Context) facade.Records {
	readers := e.readers
	if readers < 1 {
		readers = 1
	}
	buf := buffer.NewBuffer(readers, 1024, 0)
	in := e.upstream.Open(ctx)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		defer in.Close()
		for in.Next(ctx) {
			out, err := e.transform.Apply(in.Record())
			if err != nil {
				cerr = err
				return
			}
			if out == nil {
				continue // filter: transform dropped this record
			}
			if err := buf.Append(ctx, out); err != nil {
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
