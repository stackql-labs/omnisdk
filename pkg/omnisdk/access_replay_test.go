package omnisdk

import (
	"context"
	"errors"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
)

type replayAccessController bool

func (allowed replayAccessController) Authorize(context.Context, string, string) error {
	if !allowed {
		return facade.ErrAccessDenied
	}
	return nil
}

func TestReplayCannotReuseAnotherCallersAuthorization(t *testing.T) {
	calls := 0
	source := plan.NewExchangeSpec("inventory", nil, nil, func(map[string]any) facade.Operator {
		calls++
		return staticOp{rows: []map[string]any{{"id": "private"}}}
	}, nil)
	source = plan.WithAccess(source, false, facade.Permission{Resource: "relation/local.inventory", Action: "read"})
	replay := replayed{ExchangeSpec: source}
	ctx := withReplay(facade.WithAccessController(context.Background(), replayAccessController(true)))
	first := plan.Make(replay, nil).Open(ctx)
	for first.Next(ctx) {
	}
	if err := first.Err(); err != nil {
		t.Fatal(err)
	}
	first.Close()
	if calls != 1 {
		t.Fatal(calls)
	}
	other := facade.WithAccessController(ctx, replayAccessController(false))
	second := plan.Make(replay, nil).Open(other)
	defer second.Close()
	if second.Next(other) || !errors.Is(second.Err(), facade.ErrAccessDenied) || calls != 1 {
		t.Fatal("cached data escaped policy", second.Err(), calls)
	}
}
