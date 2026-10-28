package main

import (
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stackql-labs/omnisdk/public/authentication"
)

type jwtConfig struct {
	Audience   string         `json:"audience"`
	Algorithms []string       `json:"algorithms"`
	Keys       []jwtKeyConfig `json:"keys"`
}

type jwtKeyConfig struct {
	ID           string `json:"kid"`
	Algorithm    string `json:"algorithm"`
	PublicKeyPEM string `json:"public_key_pem"`
}

type verificationKey struct {
	algorithm string
	public    any
}

func configuredJWT(issuer string, cfg *jwtConfig) (authentication.Authenticator, error) {
	if cfg == nil || len(cfg.Keys) == 0 {
		return nil, errors.New("JWT verification keys are required")
	}
	allowed := map[string]bool{}
	for _, algorithm := range cfg.Algorithms {
		if (algorithm != "RS256" && algorithm != "EdDSA") || allowed[algorithm] {
			return nil, errors.New("CLI JWT algorithms must be unique RS256 or EdDSA entries")
		}
		allowed[algorithm] = true
	}
	keys := map[string]verificationKey{}
	for _, key := range cfg.Keys {
		if key.ID == "" || keys[key.ID].public != nil || !allowed[key.Algorithm] {
			return nil, errors.New("JWT key IDs must be unique and algorithms explicitly allowed")
		}
		block, rest := pem.Decode([]byte(key.PublicKeyPEM))
		if block == nil || block.Type != "PUBLIC KEY" || len(rest) != 0 {
			return nil, errors.New("JWT keys must be single PKIX PUBLIC KEY PEM blocks")
		}
		public, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid JWT public key")
		}
		switch k := public.(type) {
		case *rsa.PublicKey:
			if key.Algorithm != "RS256" || k.N.BitLen() < 2048 {
				return nil, errors.New("JWT RSA keys require RS256 and at least 2048 bits")
			}
		case ed25519.PublicKey:
			if key.Algorithm != "EdDSA" || len(k) != ed25519.PublicKeySize {
				return nil, errors.New("JWT Ed25519 keys require EdDSA")
			}
		default:
			return nil, errors.New("CLI JWT keys must be RSA or Ed25519 public keys")
		}
		keys[key.ID] = verificationKey{key.Algorithm, public}
	}
	return authentication.NewJWT(issuer, cfg.Audience, cfg.Algorithms, func(token *jwt.Token) (any, error) {
		id, ok := token.Header["kid"].(string)
		key, found := keys[id]
		if !ok || !found || token.Method.Alg() != key.algorithm {
			return nil, errors.New("unknown JWT verification key")
		}
		return key.public, nil
	})
}
