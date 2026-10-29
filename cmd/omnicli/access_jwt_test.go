package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stackql-labs/omnisdk/public/authentication"
)

func publicPEM(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestJWTConfigurationRejectsUnsafeKeys(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	weakRSA, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	key := jwtKeyConfig{ID: "one", Algorithm: "EdDSA", PublicKeyPEM: publicPEM(t, public)}
	cases := map[string]*jwtConfig{
		"missing":                  nil,
		"no audience":              {Algorithms: []string{"EdDSA"}, Keys: []jwtKeyConfig{key}},
		"no allowlist":             {Audience: "service", Keys: []jwtKeyConfig{key}},
		"unsigned":                 {Audience: "service", Algorithms: []string{"none"}, Keys: []jwtKeyConfig{key}},
		"shared secret confusion":  {Audience: "service", Algorithms: []string{"HS256"}, Keys: []jwtKeyConfig{key}},
		"duplicate algorithms":     {Audience: "service", Algorithms: []string{"EdDSA", "EdDSA"}, Keys: []jwtKeyConfig{key}},
		"duplicate key ids":        {Audience: "service", Algorithms: []string{"EdDSA"}, Keys: []jwtKeyConfig{key, key}},
		"missing key id":           {Audience: "service", Algorithms: []string{"EdDSA"}, Keys: []jwtKeyConfig{{Algorithm: "EdDSA", PublicKeyPEM: key.PublicKeyPEM}}},
		"mismatched key algorithm": {Audience: "service", Algorithms: []string{"RS256"}, Keys: []jwtKeyConfig{{ID: "one", Algorithm: "RS256", PublicKeyPEM: key.PublicKeyPEM}}},
		"weak RSA":                 {Audience: "service", Algorithms: []string{"RS256"}, Keys: []jwtKeyConfig{{ID: "weak", Algorithm: "RS256", PublicKeyPEM: publicPEM(t, &weakRSA.PublicKey)}}},
		"extra PEM":                {Audience: "service", Algorithms: []string{"EdDSA"}, Keys: []jwtKeyConfig{{ID: "one", Algorithm: "EdDSA", PublicKeyPEM: key.PublicKeyPEM + key.PublicKeyPEM}}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := configuredJWT("issuer", cfg); err == nil {
				t.Fatal("accepted invalid JWT configuration")
			}
		})
	}
	valid := &jwtConfig{Audience: "service", Algorithms: []string{"EdDSA"}, Keys: []jwtKeyConfig{key}}
	authn, err := configuredJWT("issuer", valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authn.Authenticate(context.Background(), authentication.Evidence{}); err == nil {
		t.Fatal("accepted missing credential")
	}
}
