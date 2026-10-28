package facade

import (
	"context"
	"crypto/tls"
)

// Identity is verified authentication evidence, namespaced by its issuer.
type Identity interface {
	Issuer() string
	Subject() string
}

// Principal is a resolved caller. Implementations must return immutable snapshots.
type Principal interface {
	ID() string
	Groups() []string
	Attributes() map[string]string
}

// PrincipalDirectory resolves only verified identities supplied by an authenticator.
type PrincipalDirectory interface {
	Resolve(context.Context, Identity) (Principal, error)
}

// AccessAuthorizer decides independently of provider credential authentication.
type AccessAuthorizer interface {
	Authorize(context.Context, Principal, string, string) error
}

// AccessController is the execution boundary carried through concurrent work.
type AccessController interface {
	Authorize(context.Context, string, string) error
}
type accessKey struct{}

func WithAccessController(ctx context.Context, controller AccessController) context.Context {
	return context.WithValue(ctx, accessKey{}, controller)
}
func CheckAccess(ctx context.Context, resource, action string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if controller, ok := ctx.Value(accessKey{}).(AccessController); ok {
		return controller.Authorize(ctx, resource, action)
	}
	return nil
}

type authorizedKey struct{}

// WithAuthorizedExchange marks the dynamic extent of an operation checked by a trusted compiler.
func WithAuthorizedExchange(ctx context.Context) context.Context {
	return context.WithValue(ctx, authorizedKey{}, true)
}

// CheckTransport refuses unclassified low-level effects in protected sessions. Native exchanges
// require their own explicit transport permission rather than inheriting configured credentials.
func CheckTransport(ctx context.Context, resource, action string) error {
	if allowed, _ := ctx.Value(authorizedKey{}).(bool); allowed {
		return ctx.Err()
	}
	return CheckAccess(ctx, resource, action)
}

// AuthenticationEvidence is transport-supplied evidence, not a claimed username.
type AuthenticationEvidence struct {
	Token string
	TLS   *tls.ConnectionState
}
type Authenticator interface {
	Authenticate(context.Context, AuthenticationEvidence) (Identity, error)
}
type AccessSession interface {
	Principal() Principal
	Context(context.Context) context.Context
}

func AccessEnabled(ctx context.Context) bool {
	_, ok := ctx.Value(accessKey{}).(AccessController)
	return ok
}
