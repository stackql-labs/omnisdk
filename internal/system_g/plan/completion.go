package plan

import (
	"context"
	"fmt"
	"maps"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// WithMutationCompletion follows accepted mutations before projection and wiring.
// Attach it after auth expansion, like WithProject; it never retries the mutation.
func WithMutationCompletion(spec ExchangeSpec, completion facade.MutationCompletion, address, verb string) ExchangeSpec {
	return completionSpec{ExchangeSpec: spec, completion: completion, address: address, verb: verb}
}

type completionSpec struct {
	ExchangeSpec
	completion    facade.MutationCompletion
	address, verb string
}

func (s completionSpec) Make(bound map[string]any) facade.Operator {
	return completionOp{spec: s, inner: s.ExchangeSpec.Make(bound), bound: maps.Clone(bound)}
}

type completionOp struct {
	spec  completionSpec
	inner facade.Operator
	bound map[string]any
}

func (o completionOp) Open(ctx context.Context) facade.Records {
	out := buffer.NewBuffer(1, 1, 0)
	go func() {
		var err error
		defer func() { out.Complete(err) }()
		rows := o.inner.Open(ctx)
		defer rows.Close()
		for rows.Next(ctx) {
			response, ok := bind.DocMap(rows.Record())
			if !ok {
				err = fmt.Errorf("completion: %s did not return an object", o.spec.address)
				return
			}
			var done map[string]any
			done, err = o.spec.completion.Complete(ctx, o.spec.address, o.spec.verb, maps.Clone(o.bound), maps.Clone(response))
			if err != nil {
				return
			}
			if done != nil {
				if err = out.Append(ctx, bind.NewDocRecord(done)); err != nil {
					return
				}
			}
		}
		err = rows.Err()
	}()
	return out.Reader()
}
