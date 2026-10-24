package httpx

import (
	"context"
	"crypto/tls"
	"net/http"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/readcache"
	"github.com/stackql-labs/omnisdk/internal/system_g/requestpolicy"
)

// The HTTP client is a RUN policy, carried on ctx alongside retry, admission and the result budget —
// not a constructor argument. Every exchange in a plan, hand-authored or document-compiled, then
// answers to one decision made once at the top, and no builder has to thread a client it does not
// otherwise care about.

type clientKey struct{}
type pageLimitKey struct{}

// WithPageLimit caps successful pages per resource read. Zero or negative is
// unlimited; retries and polling attempts are separate policies.
func WithPageLimit(ctx context.Context, limit int) context.Context {
	return context.WithValue(ctx, pageLimitKey{}, limit)
}

func PageLimit(ctx context.Context) int {
	limit, _ := ctx.Value(pageLimitKey{}).(int)
	return limit
}

func WithinPageLimit(ctx context.Context, fetched int) bool {
	limit := PageLimit(ctx)
	return limit <= 0 || fetched < limit
}

// DefaultRequestTimeout bounds a request, including reading its response body.
const DefaultRequestTimeout = requestpolicy.DefaultTimeout

// WithRequestTimeout sets the default deadline for each HTTP exchange, including
// reading its body. An explicit client timeout takes precedence.
func WithRequestTimeout(ctx context.Context, timeout time.Duration) context.Context {
	return requestpolicy.WithTimeout(ctx, timeout)
}

func requestContext(ctx context.Context, client *http.Client) (context.Context, context.CancelFunc) {
	if client.Timeout > 0 {
		ctx = requestpolicy.WithTimeout(ctx, client.Timeout)
	}
	return requestpolicy.Context(ctx)
}

// WithClient carries the client every exchange in this run should send with.
func WithClient(ctx context.Context, c *http.Client) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, clientKey{}, c)
}

// ClientFrom returns the run's client, or fallback when none was set (nil fallback = the default).
func ClientFrom(ctx context.Context, fallback *http.Client) *http.Client {
	if c, ok := ctx.Value(clientKey{}).(*http.Client); ok && c != nil {
		return c
	}
	if fallback != nil {
		return fallback
	}
	return http.DefaultClient
}

var (
	insecureOnce   sync.Once
	insecureClient *http.Client
)

// InsecureClient skips TLS certificate verification. It exists for ONE purpose: reaching a mock that
// serves a self-signed certificate. A caller must scope it to a run that has been retargeted at such
// a mock — skipping verification against a real provider would silently accept any certificate on the
// path, which is the failure mode the check exists to prevent.
//
// Built once and shared, so a run does not pay for a new connection pool per exchange.
func InsecureClient() *http.Client {
	insecureOnce.Do(func() {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // mock endpoints only
		insecureClient = &http.Client{Transport: t}
	})
	return insecureClient
}

type readErrorsKey struct{}

func WithReadErrors(ctx context.Context, policy facade.HTTPReadErrorPolicy) context.Context {
	if policy == nil {
		return ctx
	}
	return context.WithValue(ctx, readErrorsKey{}, policy)
}
func ignoredReadStatus(ctx context.Context, status int, body []byte) bool {
	policy, _ := ctx.Value(readErrorsKey{}).(facade.HTTPReadErrorPolicy)
	if policy == nil || !policy.IsIgnored(status) {
		return false
	}
	readcache.MarkIncomplete(ctx)
	if observer, ok := policy.(facade.HTTPReadErrorObserver); ok {
		observer.IgnoredStatus(status, string(body))
	}
	return true
}
