package docx

import (
	"context"
	"errors"
	"fmt"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/readcache"
)

type missingSelectionKey struct{ key string }

func (e missingSelectionKey) Error() string {
	return fmt.Sprintf("error processing response: unknown key %s", e.key)
}
func (e missingSelectionKey) MissingKey() string { return e.key }

type selectionReadOp struct {
	facade.Operator
	policy facade.ResponseSelectionErrorPolicy
}

func selectionRead(op facade.Operator, policy facade.ResponseSelectionErrorPolicy) facade.Operator {
	if policy == nil {
		return op
	}
	return selectionReadOp{Operator: op, policy: policy}
}
func (o selectionReadOp) Open(ctx context.Context) facade.Records {
	return &selectionReadRows{Records: o.Operator.Open(ctx), ctx: ctx, policy: o.policy}
}

type selectionReadRows struct {
	facade.Records
	ctx     context.Context
	policy  facade.ResponseSelectionErrorPolicy
	ignored bool
	done    bool
}

func (r *selectionReadRows) Next(ctx context.Context) bool {
	if r.done {
		return false
	}
	if r.Records.Next(ctx) {
		return true
	}
	r.done = true
	var selection facade.ResponseSelectionError
	if errors.As(r.Records.Err(), &selection) && r.policy.IgnoreMissingKey(selection.MissingKey()) {
		readcache.MarkIncomplete(r.ctx)
		r.ignored = true
	}
	return false
}
func (r *selectionReadRows) Err() error {
	if r.ignored {
		return nil
	}
	return r.Records.Err()
}
