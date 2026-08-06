package exchange

import (
	"context"
	"errors"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// explodeRows turns an upstream that emits records carrying an agnostic []any (a projection's
// row list) into a stream of one record per element — the fan-out that makes each row a
// tuple the bind join can drive over. Non-map elements are skipped.
type explodeRows struct {
	upstream facade.Operator
	readers  int
}

// NewExplodeRows fans a projected row-list out into one record per row.
func NewExplodeRows(upstream facade.Operator, readers int) facade.Operator {
	return explodeRows{upstream: upstream, readers: readers}
}

func (e explodeRows) Open(ctx context.Context) facade.Records {
	n := e.readers
	if n < 1 {
		n = 1
	}
	buf := buffer.NewBuffer(n, 1024, 0)
	in := e.upstream.Open(ctx)
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		defer in.Close()
		for in.Next(ctx) {
			doc, ok := value.Doc(in.Record().Get(facade.AnonymousPayload))
			if !ok {
				continue
			}
			rows, ok := doc.([]any)
			if !ok {
				continue
			}
			for _, r := range rows {
				m, ok := r.(map[string]any)
				if !ok {
					continue
				}
				if err := buf.Append(ctx, bind.NewDocRecord(m)); err != nil {
					if !errors.Is(err, buffer.ErrAllReadersClosed) {
						cerr = err
					}
					return
				}
			}
		}
		cerr = in.Err()
	}()
	return buf.Reader()
}
