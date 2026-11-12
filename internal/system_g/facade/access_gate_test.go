package facade_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type gateController func(context.Context, string, string) error

func (f gateController) Authorize(ctx context.Context, resource, action string) error {
	return f(ctx, resource, action)
}

func TestCallerChangeClearsTransportApproval(t *testing.T) {
	allowed := gateController(func(context.Context, string, string) error { return nil })
	denied := gateController(func(context.Context, string, string) error { return facade.ErrAccessDenied })
	ctx := facade.WithAccessController(context.Background(), allowed)
	ctx, err := facade.Authorize(ctx, facade.Permission{Resource: "operation/demo/list", Action: "execute"})
	if err != nil {
		t.Fatal(err)
	}
	ctx = facade.WithAuthorizedExchange(ctx)
	ctx = facade.WithAccessController(ctx, denied)
	if err := facade.CheckTransport(ctx, "transport/http/example.test", "GET"); !errors.Is(err, facade.ErrAccessDenied) {
		t.Fatal("previous caller's transport approval leaked", err)
	}
}

func TestMissingControllerAndCancelledInheritedContextFailClosed(t *testing.T) {
	ctx := facade.WithAccessController(context.Background(), nil)
	permission := facade.Permission{Resource: "collection/demo", Action: "read"}
	if _, err := facade.Authorize(ctx, permission); !errors.Is(err, facade.ErrAccessDenied) {
		t.Fatal(err)
	}
	ctx = facade.WithAccessController(context.Background(), gateController(func(context.Context, string, string) error { return nil }))
	ctx, err := facade.Authorize(ctx, permission)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := facade.Authorize(ctx, permission); !errors.Is(err, context.Canceled) {
		t.Fatal("inherited grant ignored cancellation", err)
	}
}
