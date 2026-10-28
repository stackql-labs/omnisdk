package omnisdk

import (
	"context"
	"fmt"

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
