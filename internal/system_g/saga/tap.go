package saga

import (
	"context"
	"errors"
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// tap is a passthrough operator that, as each record from a mutating exchange flows through,
// records the exchange's undo/redo intent to the run's SagaLog (from ctx), capturing keyFields
// off the row, then forwards the record unchanged. Logging only — no compensation runs.
type tap struct {
	id        int64
	upstream  facade.Operator
	exchange  string
	redo      string
	undo      string
	keyFields []string
	readers   int
}

// NewTap wraps upstream so each record it emits is logged as a saga entry (redo/undo for exchange,
// with keyFields captured from the row) before being forwarded.
func NewTap(id int64, upstream facade.Operator, exchange, redo, undo string, keyFields []string, readers int) facade.Operator {
	return &tap{id: id, upstream: upstream, exchange: exchange, redo: redo, undo: undo, keyFields: keyFields, readers: readers}
}

func (t *tap) Open(ctx context.Context) facade.Records {
	readers := t.readers
	if readers < 1 {
		readers = 1
	}
	buf := buffer.NewBuffer(readers, 1024, 0)
	log := From(ctx)
	in := t.upstream.Open(ctx)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		defer in.Close()
		for in.Next(ctx) {
			rec := in.Record()
			log.Record(facade.SagaEntry{
				Exchange: t.exchange,
				Redo:     t.redo,
				Undo:     t.undo,
				Keys:     t.keys(rec),
			})
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

// keys pulls keyFields off the record's agnostic row into the saga entry's identifiers.
func (t *tap) keys(rec facade.Record) map[string]string {
	out := make(map[string]string, len(t.keyFields))
	doc, ok := rec.Doc(facade.AnonymousPayload)
	if !ok {
		return out
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return out
	}
	for _, k := range t.keyFields {
		if v, present := m[k]; present {
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}
