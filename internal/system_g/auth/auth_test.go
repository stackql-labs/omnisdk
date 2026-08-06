package auth

import (
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

func TestParseSelectsMethod(t *testing.T) {
	cfg, err := Parse([]byte(`{"type":"client_credentials","token_url":"https://login/token","scopes":["s1","s2"],"client_id_env_var":"CID","client_secret_env_var":"CSEC"}`))
	if err != nil {
		t.Fatal(err)
	}
	if Kind(cfg.Type) != KindClientCredentials || cfg.TokenURL != "https://login/token" || len(cfg.Scopes) != 2 {
		t.Fatalf("parsed wrong: %+v", cfg)
	}
}

func TestBearerAddsAuthorizationHeader(t *testing.T) {
	t.Setenv("OIDC_TOKEN", "tok123\n") // trailing newline (file/env artifact) must be trimmed
	m, err := New(AuthStruct{Type: "bearer", CredentialsEnvVar: "OIDC_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind() != KindBearer || m.NeedsTokenExchange() {
		t.Fatalf("kind/needs wrong: %v %v", m.Kind(), m.NeedsTokenExchange())
	}
	rec, err := m.RequestTransform().Apply(httpx.NewRequestRecord("GET", "http://x", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := httpx.Header(rec).Get("Authorization"); got != "Bearer tok123" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer tok123")
	}
}

func TestAPIKeyCustomHeaderAndRequiresName(t *testing.T) {
	if _, err := New(AuthStruct{Type: "api_key", CredentialsEnvVar: "K"}); err == nil {
		t.Fatal("api_key without name should error")
	}
	t.Setenv("APIKEY", "abc")
	m, err := New(AuthStruct{Type: "api_key", Name: "X-Api-Key", CredentialsEnvVar: "APIKEY"})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := m.RequestTransform().Apply(httpx.NewRequestRecord("GET", "http://x", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := httpx.Header(rec).Get("X-Api-Key"); got != "abc" {
		t.Fatalf("X-Api-Key = %q", got)
	}
}

func TestNullIsNoOp(t *testing.T) {
	m, err := New(AuthStruct{Type: "null_auth"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind() != KindNull || m.RequestTransform() != nil || m.NeedsTokenExchange() {
		t.Fatalf("null not a no-op: %+v", m)
	}
	// empty type defaults to null too
	if m2, _ := New(AuthStruct{}); m2.Kind() != KindNull {
		t.Fatalf("empty type should default to null, got %v", m2.Kind())
	}
}

func TestClientCredentialsNeedsTokenExchange(t *testing.T) {
	t.Setenv("CID", "the-client")
	t.Setenv("CSEC", "the-secret")
	m, err := New(AuthStruct{Type: "client_credentials", TokenURL: "https://login/token",
		Scopes: []string{"s"}, ClientIDEnvVar: "CID", ClientSecretEnvVar: "CSEC"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.NeedsTokenExchange() || m.RequestTransform() != nil {
		t.Fatal("oauth method must need a token exchange and add no request transform itself")
	}
	tr, ok := AsOAuth(m)
	if !ok || tr.TokenURL != "https://login/token" || tr.ClientID != "the-client" || tr.ClientSecret != "the-secret" {
		t.Fatalf("token request wrong: %+v ok=%v", tr, ok)
	}
}

func TestSigV4NotFromConfigButWrappable(t *testing.T) {
	if _, err := New(AuthStruct{Type: "aws_signing_v4"}); err == nil {
		t.Fatal("aws_signing_v4 from config should error (region-scoped)")
	}
	m := FromTransform(KindSigV4, headerT{name: "Authorization", value: "AWS4-..."})
	if m.Kind() != KindSigV4 || m.RequestTransform() == nil {
		t.Fatalf("wrapped sigv4 wrong: %+v", m)
	}
}
