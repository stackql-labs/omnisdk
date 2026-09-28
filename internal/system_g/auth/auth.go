// Package auth makes authentication a config-driven, switchable, cross-cutting concern. Auth has two
// physical shapes and both satisfy Method: a pure request transform (AWS SigV4; a static/OIDC bearer
// header) and a bearer token obtained from a prior token exchange (GCP service account, Azure/Entra
// or generic OIDC client-credentials). The wire config is AuthStruct — a massively pared-back subset
// of stackql's any-sdk dto auth semantics — so an integration can swap methods by JSON alone.
package auth

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/secret"
)

// Kind is the resolved auth method.
type Kind string

const (
	KindNull              Kind = "null_auth"
	KindBearer            Kind = "bearer"
	KindAPIKey            Kind = "api_key"
	KindSigV4             Kind = "aws_signing_v4"
	KindServiceAccount    Kind = "service_account"
	KindClientCredentials Kind = "client_credentials"
	KindBasic             Kind = "basic"
	// KindCustom is stackql's name for a key in a named header or query parameter: api_key by another
	// name.
	KindCustom Kind = "custom"
	// KindKubeconfig reads a kubeconfig context: its user's token, token file, client certificate or
	// exec plugin, and its cluster's certificate authority.
	KindKubeconfig Kind = "kubeconfig"
)

// AuthStruct is the pared-back stackql auth config: the JSON that SELECTS a method and carries its
// inputs. Only the fields this SDK needs are kept. Every credential resolves inline value → env var →
// file, in that order: inline lets a programmatic caller (e.g. stackql) inject secrets per-request;
// the env/file names default to the provider's canonical vars when omitted.
type AuthStruct struct {
	Type string `json:"type"`

	// Credential-blob sourcing (SA JSON, api key, static/OIDC token) — inline value, env var, or file.
	Credentials         string `json:"credentials,omitempty"`
	CredentialsEnvVar   string `json:"credentialsenvvar,omitempty"`
	CredentialsFilePath string `json:"credentialsfilepath,omitempty"`

	// Header injection (bearer, api_key).
	ValuePrefix string `json:"valuePrefix,omitempty"` // e.g. "Bearer " (default for bearer)
	Name        string `json:"name,omitempty"`        // header name (default Authorization)
	// Location is where an api_key goes: "header" (default) or "query".
	Location string `json:"location,omitempty"`

	// Basic: a username and password (inline, else env), or else Credentials holding the pair already
	// base64-encoded, as stackql's basic credentials are.
	Username       string `json:"username,omitempty"`
	Password       string `json:"password,omitempty"`
	UsernameEnvVar string `json:"username_var,omitempty"`
	PasswordEnvVar string `json:"password_var,omitempty"`

	// TLS the connection needs: a certificate authority to trust, a client certificate to present.
	TLS *TLS `json:"tls,omitempty"`
	// Context names the kubeconfig context to read (kubeconfig); required, never the current one by
	// default — which cluster's credentials are sent is not a guess.
	Context string `json:"context,omitempty"`
	// Successor is a further method applied after this one, e.g. a second API-key header.
	Successor *AuthStruct `json:"successor,omitempty"`

	// OAuth2 token exchange (client_credentials for Azure/Entra & generic OIDC).
	Scopes             []string `json:"scopes,omitempty"`
	TokenURL           string   `json:"token_url,omitempty"`
	ClientID           string   `json:"client_id,omitempty"`     // inline; else ClientIDEnvVar
	ClientSecret       string   `json:"client_secret,omitempty"` // inline; else ClientSecretEnvVar
	ClientIDEnvVar     string   `json:"client_id_env_var,omitempty"`
	ClientSecretEnvVar string   `json:"client_secret_env_var,omitempty"`
}

// Parse reads an AuthStruct from JSON.
func Parse(b []byte) (AuthStruct, error) {
	var a AuthStruct
	if err := json.Unmarshal(b, &a); err != nil {
		return AuthStruct{}, fmt.Errorf("auth: parse: %w", err)
	}
	return a, nil
}

// Method is the config-selected auth mechanism (the switchable seam).
type Method interface {
	Kind() Kind
	// RequestTransform decorates each outgoing request (sign, or add the auth header); nil = no-op.
	// Self-contained for SigV4 and static/OIDC bearer. For token-exchange methods it is nil — the
	// bearer header is added by the token exchange's own β wiring, not here.
	RequestTransform() facade.Transform
	// NeedsTokenExchange reports whether a prior token-acquisition call is required (OAuth flows).
	NeedsTokenExchange() bool
}

// TokenRequest is the resolved input for an OAuth2 token exchange (for callers that wire it).
type TokenRequest struct {
	TokenURL     string
	Scopes       []string
	ClientID     string // client_credentials
	ClientSecret string // client_credentials
	Credentials  string // service_account: the SA key JSON
}

// New builds the Method for a config. Self-contained methods (null, bearer, api_key) are complete;
// oauth methods (service_account, client_credentials) resolve their token inputs and report
// NeedsTokenExchange — use AsOAuth to get the inputs. SigV4 is region-scoped, so build it with
// FromTransform(KindSigV4, signerTransform) rather than from this config.
func New(cfg AuthStruct) (Method, error) {
	m, err := newMethod(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.TLS != nil {
		tc, err := cfg.TLS.config()
		if err != nil {
			return nil, err
		}
		m = withTLS(m, tc)
	}
	if cfg.Successor == nil {
		return m, nil
	}
	next, err := New(*cfg.Successor)
	if err != nil {
		return nil, fmt.Errorf("auth: successor: %w", err)
	}
	return chain(m, next)
}

func newMethod(cfg AuthStruct) (Method, error) {
	switch Kind(cfg.Type) {
	case KindNull, "":
		return static{kind: KindNull}, nil
	case KindBearer:
		tok, err := cred(cfg)
		if err != nil {
			return nil, err
		}
		return static{kind: KindBearer, t: headerT{name: def(cfg.Name, "Authorization"), value: def(cfg.ValuePrefix, "Bearer ") + tok}, raw: tok}, nil
	case KindAPIKey, KindCustom:
		if cfg.Name == "" {
			return nil, fmt.Errorf("auth: %s requires name", cfg.Type)
		}
		tok, err := cred(cfg)
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(def(cfg.Location, "header")) {
		case "header":
			return static{kind: KindAPIKey, t: headerT{name: cfg.Name, value: cfg.ValuePrefix + tok}}, nil
		case "query":
			return static{kind: KindAPIKey, t: queryT{name: cfg.Name, value: cfg.ValuePrefix + tok}}, nil
		}
		return nil, fmt.Errorf("auth: %s location %q is neither header nor query", cfg.Type, cfg.Location)
	case KindBasic:
		enc, err := basicCredential(cfg)
		if err != nil {
			return nil, err
		}
		return static{kind: KindBasic, t: headerT{name: def(cfg.Name, "Authorization"), value: def(cfg.ValuePrefix, "Basic ") + enc}}, nil
	case KindKubeconfig:
		return kubeconfig(cfg)
	case KindClientCredentials, KindServiceAccount:
		tr, err := tokenRequest(cfg)
		if err != nil {
			return nil, err
		}
		return oauth{kind: Kind(cfg.Type), req: tr}, nil
	case KindSigV4:
		return nil, fmt.Errorf("auth: %s is region-scoped — build it with FromTransform", KindSigV4)
	default:
		return nil, fmt.Errorf("auth: unknown type %q", cfg.Type)
	}
}

// FromTransform wraps an existing request transform (e.g. a SigV4 signer) as a Method, so provider
// code with region/service context still presents through the same seam.
func FromTransform(kind Kind, t facade.Transform) Method { return static{kind: kind, t: t} }

// AsOAuth returns the token-exchange inputs if m is an OAuth method.
func AsOAuth(m Method) (TokenRequest, bool) {
	o, ok := m.(oauth)
	return o.req, ok
}

// BearerToken returns the resolved static token of a KindBearer method, for callers that inject it
// as a value (a κ {token}) rather than as a per-request transform. false for any other method.
func BearerToken(m Method) (string, bool) {
	s, ok := m.(static)
	if !ok || s.kind != KindBearer {
		return "", false
	}
	return s.raw, true
}

// ClientCredentialsRequest builds the OAuth2 client-credentials token POST (Azure/Entra, generic
// OIDC). Inputs are baked from the resolved TokenRequest; the response's access_token is the bearer
// token. Callers wrap it as a token-acquisition exchange whose output feeds {token} downstream.
func ClientCredentialsRequest(tr TokenRequest) httpx.Request {
	return httpx.Request{
		Method: "POST", URL: tr.TokenURL,
		Body: httpx.Body{Encoding: httpx.EncodingForm, Params: map[string]any{
			"grant_type":    "client_credentials",
			"client_id":     tr.ClientID,
			"client_secret": tr.ClientSecret,
			"scope":         strings.Join(tr.Scopes, " "),
		}},
	}
}

type static struct {
	kind Kind
	t    facade.Transform
	raw  string // the resolved token for KindBearer (see BearerToken)
}

func (m static) Kind() Kind                         { return m.kind }
func (m static) RequestTransform() facade.Transform { return m.t }
func (m static) NeedsTokenExchange() bool           { return false }

type oauth struct {
	kind Kind
	req  TokenRequest
}

func (m oauth) Kind() Kind                         { return m.kind }
func (m oauth) RequestTransform() facade.Transform { return nil }
func (m oauth) NeedsTokenExchange() bool           { return true }

// headerT adds a fixed header to the request record (the same rebuild SigV4 does).
type headerT struct{ name, value string }

func (t headerT) Apply(in facade.Page) (facade.Record, error) {
	h := httpx.Header(in)
	h.Set(t.name, t.value)
	return httpx.NewRequestRecord(httpx.Method(in), httpx.URL(in), h, httpx.ReqBody(in)), nil
}

// queryT sets a query parameter on each outgoing request.
type queryT struct{ name, value string }

func (t queryT) Apply(in facade.Page) (facade.Record, error) {
	u, err := url.Parse(httpx.URL(in))
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	q := u.Query()
	q.Set(t.name, t.value)
	u.RawQuery = q.Encode()
	return httpx.NewRequestRecord(httpx.Method(in), u.String(), httpx.Header(in), httpx.ReqBody(in)), nil
}

// basicCredential is the base64 user:password pair: from a username and password where both
// resolve, else the credential blob, which stackql's convention holds already encoded.
func basicCredential(cfg AuthStruct) (string, error) {
	user := secret.Optional(secret.Literal(cfg.Username), secret.Env(cfg.UsernameEnvVar))
	pass := secret.Optional(secret.Literal(cfg.Password), secret.Env(cfg.PasswordEnvVar))
	if user != "" && pass != "" {
		return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)), nil
	}
	enc, err := cred(cfg)
	if err != nil {
		return "", fmt.Errorf("auth: basic needs a username and password, or an encoded credential: %w", err)
	}
	return enc, nil
}

// cred resolves the credential blob (token / api key / SA JSON): inline value → env var → file.
func cred(cfg AuthStruct) (string, error) {
	v, err := secret.Require("auth credential",
		secret.Literal(cfg.Credentials), secret.Env(cfg.CredentialsEnvVar), secret.File(cfg.CredentialsFilePath)).Resolve()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

func tokenRequest(cfg AuthStruct) (TokenRequest, error) {
	tr := TokenRequest{TokenURL: cfg.TokenURL, Scopes: cfg.Scopes}
	if Kind(cfg.Type) == KindServiceAccount {
		c, err := cred(cfg)
		if err != nil {
			return TokenRequest{}, err
		}
		tr.Credentials = c
		return tr, nil
	}
	// client_credentials: id + secret inline → env (secrets, optional here — validated when wired).
	tr.ClientID = secret.Optional(secret.Literal(cfg.ClientID), secret.Env(cfg.ClientIDEnvVar))
	tr.ClientSecret = secret.Optional(secret.Literal(cfg.ClientSecret), secret.Env(cfg.ClientSecretEnvVar))
	return tr, nil
}

func def(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// TLSConfigurer is a Method that needs its own TLS: a CA to trust, a client certificate. A caller
// sends that method's requests through a client carrying it.
type TLSConfigurer interface {
	TLSConfig() *tls.Config
}

type tlsMethod struct {
	Method
	tc *tls.Config
}

func (m tlsMethod) TLSConfig() *tls.Config { return m.tc }

func withTLS(m Method, tc *tls.Config) Method {
	if prev, ok := m.(TLSConfigurer); ok {
		tc = mergeTLS(prev.TLSConfig(), tc)
	}
	return tlsMethod{Method: m, tc: tc}
}

// mergeTLS lets explicit settings in b override a, e.g. a caller's CA over a kubeconfig's.
func mergeTLS(a, b *tls.Config) *tls.Config {
	out := a.Clone()
	if b.RootCAs != nil {
		out.RootCAs = b.RootCAs
	}
	if len(b.Certificates) > 0 {
		out.Certificates = b.Certificates
	}
	out.InsecureSkipVerify = out.InsecureSkipVerify || b.InsecureSkipVerify
	return out
}

// chain applies first's request transform, then next's. A token exchange cannot be chained: its
// header comes from another call, not from this one.
func chain(first, next Method) (Method, error) {
	if first.NeedsTokenExchange() || next.NeedsTokenExchange() {
		return nil, fmt.Errorf("auth: a successor cannot follow or be a token exchange")
	}
	var ts []facade.Transform
	for _, m := range []Method{first, next} {
		if t := m.RequestTransform(); t != nil {
			ts = append(ts, t)
		}
	}
	var t facade.Transform
	if len(ts) > 0 {
		t = chainT(ts)
	}
	var out Method = static{kind: first.Kind(), t: t}
	for _, m := range []Method{first, next} {
		if tc, ok := m.(TLSConfigurer); ok {
			out = withTLS(out, tc.TLSConfig())
		}
	}
	return out, nil
}

type chainT []facade.Transform

func (c chainT) Apply(in facade.Page) (facade.Record, error) {
	var rec facade.Record
	for _, t := range c {
		out, err := t.Apply(in)
		if err != nil {
			return nil, err
		}
		rec, in = out, out
	}
	return rec, nil
}

// TLS is the transport security a connection needs. Each of CA, certificate and key comes from a
// file or from base64 PEM data; data wins.
type TLS struct {
	CACertFile         string `json:"ca_cert_file,omitempty"`
	CACertData         string `json:"ca_cert_data,omitempty"`
	ClientCertFile     string `json:"client_cert_file,omitempty"`
	ClientCertData     string `json:"client_cert_data,omitempty"`
	ClientKeyFile      string `json:"client_key_file,omitempty"`
	ClientKeyData      string `json:"client_key_data,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
}

func (t TLS) config() (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: t.InsecureSkipVerify}
	ca, err := pemFrom("certificate authority", t.CACertData, t.CACertFile)
	if err != nil {
		return nil, err
	}
	if ca != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("auth: certificate authority holds no PEM certificate")
		}
		tc.RootCAs = pool
	}
	cert, err := pemFrom("client certificate", t.ClientCertData, t.ClientCertFile)
	if err != nil {
		return nil, err
	}
	key, err := pemFrom("client key", t.ClientKeyData, t.ClientKeyFile)
	if err != nil {
		return nil, err
	}
	switch {
	case cert != nil && key != nil:
		pair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			return nil, fmt.Errorf("auth: client certificate: %w", err)
		}
		tc.Certificates = []tls.Certificate{pair}
	case cert != nil || key != nil:
		return nil, fmt.Errorf("auth: a client certificate needs both certificate and key")
	}
	return tc, nil
}

// pemFrom reads PEM from base64 data, else a file; nil where neither is given.
func pemFrom(what, data, file string) ([]byte, error) {
	if data != "" {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data))
		if err != nil {
			return nil, fmt.Errorf("auth: %s data is not base64: %w", what, err)
		}
		return b, nil
	}
	if file == "" {
		return nil, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("auth: %s: %w", what, err)
	}
	return b, nil
}
