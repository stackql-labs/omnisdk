package omnisdk

import (
	"context"
	"fmt"
	"slices"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/public/authorization"
)

func executionContext(ctx context.Context, args Args) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if args.Caller != nil {
		ctx = args.Caller.Context(ctx)
	}
	if (args.RequireCaller || args.Caller != nil) && !facade.AccessEnabled(ctx) {
		return nil, fmt.Errorf("%w: authenticated caller is required", authorization.ErrDenied)
	}
	return ctx, nil
}

// authorizeExecution establishes the caller once and applies the shared permission gate.
func authorizeExecution(ctx context.Context, args Args, permissions ...facade.Permission) (context.Context, error) {
	ctx, err := executionContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return facade.Authorize(ctx, permissions...)
}

// WithPermissions gives a host-provided queryable plan the same inherited authorization boundary.
// In protected sessions an empty/incomplete declaration is denied before the inner plan opens.
func WithPermissions(inner Plan, args Args, permissions ...authorization.Permission) Plan {
	return accessPlan{Plan: inner, args: args, permissions: slices.Clone(permissions)}
}

type accessPlan struct {
	Plan
	args        Args
	permissions []facade.Permission
}

func (p accessPlan) Open(ctx context.Context) (Rows, error) {
	ctx, err := authorizeExecution(ctx, p.args, p.permissions...)
	if err != nil {
		return nil, err
	}
	if p.Plan == nil {
		return nil, fmt.Errorf("omnisdk: missing queryable plan")
	}
	return p.Plan.Open(ctx)
}
