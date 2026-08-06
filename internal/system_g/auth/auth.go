// Package auth makes authentication a config-driven, switchable, cross-cutting concern. Auth has two
// physical shapes and both satisfy Method: a pure request transform (AWS SigV4; a static/OIDC bearer
// header) and a bearer token obtained from a prior token exchange (GCP service account, Azure/Entra
// or generic OIDC client-credentials). The wire config is AuthStruct — a massively pared-back subset
// of stackql's any-sdk dto auth semantics — so an integration can swap methods by JSON alone.
package auth

import (
	"encoding/json"
	"fmt"
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
	switch Kind(cfg.Type) {
	case KindNull, "":
		return static{kind: KindNull}, nil
	case KindBearer:
		tok, err := cred(cfg)
		if err != nil {
			return nil, err
		}
		return static{kind: KindBearer, t: headerT{name: def(cfg.Name, "Authorization"), value: def(cfg.ValuePrefix, "Bearer ") + tok}, raw: tok}, nil
	case KindAPIKey:
		if cfg.Name == "" {
			return nil, fmt.Errorf("auth: api_key requires name")
		}
		tok, err := cred(cfg)
		if err != nil {
			return nil, err
		}
		return static{kind: KindAPIKey, t: headerT{name: cfg.Name, value: cfg.ValuePrefix + tok}}, nil
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
