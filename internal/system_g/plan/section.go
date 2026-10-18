package plan

import (
	"context"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/schedule"
)

// section runs independent stages side by side. For each upstream row every member runs on that row
// at once, under the run's section limit; the rows it emits are every combination of the members'
// rows, each merged over the upstream one — what the stages would emit one after another, since none
// reads what another writes.
//
// A failure starts nothing more: a member still waiting for a slot does not run, and no further row
// is taken. A member already running is not cancelled, because an effect abandoned mid-request has
// an unknown outcome; it finishes, and the first failure is reported once every member has stopped.
type section struct {
	up      facade.Operator
	slots   []*rowSlot
	members []facade.Operator
}

// rowSlot is a member's upstream: the one row its section is running, set before the member opens.
type rowSlot struct{ rec facade.Record }

func (s *rowSlot) Open(ctx context.Context) facade.Records { return seedOp{rec: s.rec}.Open(ctx) }

func (s *section) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1024, buffer.Ahead(1, buffer.RowsAhead(ctx)))
	go func() {
		var err error
		defer func() { buf.Complete(err) }()
		rows := s.up.Open(ctx)
		defer rows.Close()
		for rows.Next(ctx) {
			base, ok := bind.DocMap(rows.Record())
			if !ok {
				continue
			}
			var outs [][]map[string]any
			if outs, err = s.run(ctx, rows.Record()); err != nil {
				return
			}
			for _, m := range combine(base, outs) {
				if err = buf.Append(ctx, bind.NewDocRecord(m)); err != nil {
					return
				}
			}
		}
		err = rows.Err()
	}()
	return buf.Reader()
}

// run runs every member on rec and returns the rows each emitted, in member order.
func (s *section) run(ctx context.Context, rec facade.Record) ([][]map[string]any, error) {
	for _, slot := range s.slots {
		slot.rec = rec
	}
	lim := schedule.SectionLimiterFrom(ctx)
	outs := make([][]map[string]any, len(s.members))
	var mu sync.Mutex
	var first error
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return first != nil
	}
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if first == nil {
			first = err
		}
	}
	// Slots are taken in member order, here rather than by each member racing for one, so members
	// start in the order they were declared and none starts once a failure is known.
	var wg sync.WaitGroup
	for i, m := range s.members {
		var tok facade.Token
		if lim != nil {
			var err error
			if tok, err = lim.Acquire(ctx); err != nil {
				fail(err)
				break
			}
		}
		if failed() {
			if tok != nil {
				tok.Release()
			}
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok != nil {
				defer tok.Release()
			}
			recs := m.Open(ctx)
			defer recs.Close()
			for recs.Next(ctx) {
				if row, ok := bind.DocMap(recs.Record()); ok {
					outs[i] = append(outs[i], row)
				}
			}
			if err := recs.Err(); err != nil {
				fail(err)
			}
		}()
	}
	wg.Wait()
	return outs, first
}

// combine is every way of taking one row from each member, merged over base in member order. A
// member that emitted nothing leaves nothing to combine, as an inner join does.
func combine(base map[string]any, outs [][]map[string]any) []map[string]any {
	acc := []map[string]any{base}
	for _, rows := range outs {
		var next []map[string]any
		for _, a := range acc {
			for _, r := range rows {
				m := make(map[string]any, len(a)+len(r))
				for k, v := range a {
					m[k] = v
				}
				for k, v := range r {
					m[k] = v
				}
				next = append(next, m)
			}
		}
		acc = next
	}
	return acc
}
