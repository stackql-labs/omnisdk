package exchange

import (
	"context"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

var (
	_ facade.Operator = literalSource{}
)

// literalSource is a trivial seed operator that emits a fixed set of records once. It only
// needs to be an Operator (transform/send nodes pull from an Operator), so it carries none of
// the wiring boilerplate.
type literalSource struct {
	recs    []facade.Record
	readers int
}

// NewLiteralSource yields recs once when opened.
func NewLiteralSource(recs []facade.Record, readers int) facade.Operator {
	return literalSource{recs: recs, readers: readers}
}

func (s literalSource) Open(ctx context.Context) facade.Records {
	n := s.readers
	if n < 1 {
		n = 1
	}
	buf := buffer.NewBuffer(n, 1024, 0)
	go func() {
		defer buf.Complete(nil)
		for _, r := range s.recs {
			if err := buf.Append(ctx, r); err != nil {
				return
			}
		}
	}()
	return buf.Reader()
}
