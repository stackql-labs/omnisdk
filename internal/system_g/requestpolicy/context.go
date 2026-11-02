package requestpolicy

import (
	"context"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type key struct{}
type timeoutKey struct{}

// DefaultTimeout is the fallback request deadline, including response consumption.
const DefaultTimeout = 30 * time.Second

// WithTimeout sets the request duration; zero uses the default, negative disables it.
func WithTimeout(ctx context.Context, timeout time.Duration) context.Context {
	return context.WithValue(ctx, timeoutKey{}, timeout)
}

// Context starts a fresh request deadline, bounded by the parent deadline.
func Context(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout, _ := ctx.Value(timeoutKey{}).(time.Duration)
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return ctx, func() {}
}

// WithPolicy carries the run's provider policy resolver.
func WithPolicy(ctx context.Context, p facade.RequestPolicy) context.Context {
	return context.WithValue(ctx, key{}, p)
}

// ForProvider applies the named provider's policy, retaining the parent deadline.
type tenancyKey struct{}
type tenancy struct{ provider, name string }

func WithTenancy(ctx context.Context, provider, name string) context.Context {
	return context.WithValue(ctx, tenancyKey{}, tenancy{provider, name})
}
func ForProvider(ctx context.Context, provider string) context.Context {
	if scope, ok := ctx.Value(tenancyKey{}).(tenancy); ok && (provider == scope.provider || provider == "stackql_unstable_"+scope.provider) {
		provider = scope.provider + "\x00" + scope.name
	}
	if p, ok := ctx.Value(key{}).(facade.RequestPolicy); ok && provider != "" {
		return p.ForProvider(ctx, provider)
	}
	return ctx
}
