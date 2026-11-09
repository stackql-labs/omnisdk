package plan

import (
	"context"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// WithCheckpoint reports successful exhaustion of a whole stage, not each
// request or row. A section reports only after all its upstream rows finish.
func WithCheckpoint(spec ExchangeSpec, done func(context.Context) error) ExchangeSpec {
	return checkpointSpec{ExchangeSpec: spec, done: done}
}

type checkpointSpec struct {
	ExchangeSpec
	done func(context.Context) error
}

func (s checkpointSpec) checkpoint(ctx context.Context) error { return s.done(ctx) }

type checkpointOp struct {
	facade.Operator
	callbacks []func(context.Context) error
}

func (o checkpointOp) Open(ctx context.Context) facade.Records {
	return &checkpointRecords{Records: o.Operator.Open(ctx), callbacks: o.callbacks}
}

type checkpointRecords struct {
	facade.Records
	callbacks []func(context.Context) error
	loaded    bool
	rows      []facade.Record
	position  int
	err       error
}

// A managed stage is a barrier: exhaust all its invocations and durably latch
// completion before publishing rows into the next stage. A streaming EOF hook
// would run only after downstream work, retaining locks until the whole run ended.
func (r *checkpointRecords) Next(ctx context.Context) bool {
	if !r.loaded {
		r.loaded = true
		for r.Records.Next(ctx) {
			r.rows = append(r.rows, r.Records.Record())
		}
		r.err = r.Records.Err()
		if r.err == nil {
			r.err = ctx.Err()
		}
		if r.err == nil {
			for _, callback := range r.callbacks {
				if r.err = callback(ctx); r.err != nil {
					break
				}
			}
		}
	}
	if r.err != nil || r.position >= len(r.rows) {
		return false
	}
	r.position++
	return true
}
func (r *checkpointRecords) Record() facade.Record { return r.rows[r.position-1] }
func (r *checkpointRecords) Err() error            { return r.err }
