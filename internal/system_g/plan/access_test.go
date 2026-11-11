package plan_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
)

type allowGate struct{}

func (allowGate) Authorize(context.Context, string, string) error { return nil }

func TestUnclassifiedExchangeCannotInvokeFactoryUnderWildcardPolicy(t *testing.T) {
	calls := 0
	spec := plan.NewExchangeSpec("unclassified", nil, nil, func(map[string]any) facade.Operator { calls++; panic("denied exchange invoked factory") }, nil)
	gated := plan.WithAccess(spec, true)
	rows := gated.Make(nil).Open(facade.WithAccessController(context.Background(), allowGate{}))
	defer rows.Close()
	if rows.Next(context.Background()) || !errors.Is(rows.Err(), facade.ErrAccessDenied) || calls != 0 {
		t.Fatal("unclassified factory ran", rows.Err(), calls)
	}
}

func TestSharedExecutorRejectsUndeclaredExchangeWithoutOptInWrapper(t *testing.T) {
	calls := 0
	spec := plan.NewExchangeSpec("forgotten", nil, nil, func(map[string]any) facade.Operator { calls++; panic("unclassified factory ran") }, nil)
	execution := plan.ComposeRows(1, plan.NewPlan([]plan.ExchangeSpec{spec}, nil, nil, nil, nil, nil))
	ctx := facade.WithAccessController(context.Background(), allowGate{})
	rows := execution.Open(ctx)
	defer rows.Close()
	for rows.Next(ctx) {
	}
	if !errors.Is(rows.Err(), facade.ErrAccessDenied) || calls != 0 {
		t.Fatal("executor did not fail closed", rows.Err(), calls)
	}
}
