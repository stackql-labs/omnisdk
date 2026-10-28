// Package authentication verifies caller credentials, independently of cloud credentials.
package authentication

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"os/user"

	"github.com/golang-jwt/jwt/v5"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/public/principal"
)

// Evidence is supplied by the application transport. TLS must be the actual connection state.
type Evidence = facade.AuthenticationEvidence
type Authenticator = facade.Authenticator

type authenticator func(context.Context, Evidence) (principal.Identity, error)

func (a authenticator) Authenticate(ctx context.Context, e Evidence) (principal.Identity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a(ctx, e)
}

// Token is a provisioned static bearer credential. Digest is a SHA-256 digest, never plaintext.
type Token struct {
	Subject string
	Digest  []byte
}

func NewBearer(issuer string, tokens []Token) (Authenticator, error) {
	if issuer == "" || len(tokens) == 0 {
		return nil, errors.New("bearer issuer and tokens are required")
	}
	copied := make([]Token, len(tokens))
	seen := map[[32]byte]bool{}
	for n, t := range tokens {
		if t.Subject == "" || len(t.Digest) != 32 {
			return nil, errors.New("invalid bearer credential")
		}
		var key [32]byte
		copy(key[:], t.Digest)
		if seen[key] {
			return nil, errors.New("ambiguous bearer credential")
		}
		seen[key] = true
		copied[n] = Token{t.Subject, append([]byte(nil), t.Digest...)}
	}
	return authenticator(func(_ context.Context, e Evidence) (principal.Identity, error) {
		sum := sha256.Sum256([]byte(e.Token))
		subject := ""
		for _, t := range copied {
			if subtle.ConstantTimeCompare(sum[:], t.Digest) == 1 {
				subject = t.Subject
			}
		}
		if e.Token == "" || subject == "" {
			return nil, errors.New("invalid bearer credential")
		}
		return principal.NewIdentity(issuer, subject)
	}), nil
}

// NewJWT requires an explicit algorithm allowlist, issuer, audience and verification key resolver.
func NewJWT(issuer, audience string, algorithms []string, keys jwt.Keyfunc) (Authenticator, error) {
	if issuer == "" || audience == "" || len(algorithms) == 0 || keys == nil {
		return nil, errors.New("JWT issuer, audience, algorithms and keys are required")
	}
	for _, a := range algorithms {
		if a == "none" || jwt.GetSigningMethod(a) == nil {
			return nil, errors.New("invalid JWT algorithm")
		}
	}
	parser := jwt.NewParser(jwt.WithValidMethods(append([]string(nil), algorithms...)), jwt.WithIssuer(issuer), jwt.WithAudience(audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	return authenticator(func(_ context.Context, e Evidence) (principal.Identity, error) {
		claims := jwt.RegisteredClaims{}
		t, err := parser.ParseWithClaims(e.Token, &claims, keys)
		if err != nil || !t.Valid {
			return nil, errors.New("invalid JWT credential")
		}
		return principal.NewIdentity(issuer, claims.Subject)
	}), nil
}

// NewMTLS independently verifies client authentication against explicitly configured roots.
// identity selects a SAN according to application policy; it must not trust an unverified certificate.
func NewMTLS(issuer string, roots *x509.CertPool, identity func(*x509.Certificate) (string, error)) (Authenticator, error) {
	if issuer == "" || roots == nil || identity == nil {
		return nil, errors.New("mTLS issuer, roots and identity mapping are required")
	}
	roots = roots.Clone()
	return authenticator(func(_ context.Context, e Evidence) (principal.Identity, error) {
		if e.TLS == nil || !e.TLS.HandshakeComplete || len(e.TLS.PeerCertificates) == 0 {
			return nil, errors.New("client certificate is required")
		}
		inter := x509.NewCertPool()
		for _, c := range e.TLS.PeerCertificates[1:] {
			inter.AddCert(c)
		}
		cert := e.TLS.PeerCertificates[0]
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			return nil, errors.New("invalid client certificate")
		}
		subject, err := identity(cert)
		if err != nil {
			return nil, err
		}
		return principal.NewIdentity(issuer, subject)
	}), nil
}

// NewLocalOS authenticates this process's effective OS user, never a remote supplied username.
// Directory membership remains authoritative; OS groups are resolved by the application's directory.
func NewLocalOS(issuer string) (Authenticator, error) {
	if issuer == "" {
		return nil, errors.New("OS issuer is required")
	}
	return authenticator(func(_ context.Context, e Evidence) (principal.Identity, error) {
		if e.Token != "" || e.TLS != nil {
			return nil, errors.New("local OS authentication accepts no remote evidence")
		}
		var u *user.User
		var err error
		if uid := os.Geteuid(); uid >= 0 {
			u, err = user.LookupId(fmt.Sprint(uid))
		} else {
			u, err = user.Current()
		}
		if err != nil {
			return nil, err
		}
		return principal.NewIdentity(issuer, u.Uid)
	}), nil
}
