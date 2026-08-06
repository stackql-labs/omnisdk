package schedule

import (
	"context"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/admit"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Sub is one disjoint DAG and its own admission controller (nil = inherit the run's).
type Sub struct {
	Op         facade.Operator
	Admissions facade.Admissions
}

// NewMulti runs several disjoint DAGs concurrently, each under its own admission controller,
// into their own sinks. Each sub is a sink (it writes its own output), so this just drives them
// all to completion and reports the first error. The composition point for a multi-provider run.
func NewMulti(subs []Sub) facade.Operator { return multi{subs: subs} }

type multi struct{ subs []Sub }

func (m multi) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1, 0)
	go func() {
		var wg sync.WaitGroup
		var mu sync.Mutex
		var firstErr error
		for _, s := range m.subs {
			wg.Add(1)
			go func(s Sub) {
				defer wg.Done()
				c := ctx
				if s.Admissions != nil {
					c = admit.WithAdmissions(ctx, s.Admissions)
				}
				rs := s.Op.Open(c)
				defer rs.Close()
				for rs.Next(c) { //nolint // sub is a sink; ranging drives it to EOF
				}
				if err := rs.Err(); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}(s)
		}
		wg.Wait()
		buf.Complete(firstErr)
	}()
	return buf.Reader()
}
