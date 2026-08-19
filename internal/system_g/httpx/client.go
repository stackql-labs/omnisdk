package httpx

import (
	"context"
	"crypto/tls"
	"net/http"
	"sync"
)

// The HTTP client is a RUN policy, carried on ctx alongside retry, admission and the result budget —
// not a constructor argument. Every exchange in a plan, hand-authored or document-compiled, then
// answers to one decision made once at the top, and no builder has to thread a client it does not
// otherwise care about.

type clientKey struct{}

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
