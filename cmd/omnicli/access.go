package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/stackql-labs/omnisdk/public/authentication"
	"github.com/stackql-labs/omnisdk/public/authorization"
	"github.com/stackql-labs/omnisdk/public/principal"
)

// accessConfig is CLI-owned serde, including directory and policy snapshots. Re-read on each CLI
// invocation; no global principal, directory cache, policy reload or provider credential inheritance.
type accessConfig struct {
	Issuer    string               `json:"issuer"`
	Mechanism string               `json:"mechanism"`
	Directory json.RawMessage      `json:"directory"`
	JWT       *jwtConfig           `json:"jwt,omitempty"`
	Tokens    []tokenConfig        `json:"tokens,omitempty"`
	Rules     []authorization.Rule `json:"rules"`
}
type tokenConfig struct {
	Subject string `json:"subject"`
	SHA256  string `json:"sha256"`
}

func accessContext(ctx context.Context, path, tokenEnv string) (context.Context, error) {
	if path == "" {
		if tokenEnv != "" {
			return nil, errors.New("--caller-token-env requires --access-config")
		}
		return ctx, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg accessConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("access configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("access configuration has trailing data")
	}
	directory, err := principal.NewMemoryDirectoryFromBytes(cfg.Directory)
	if err != nil {
		return nil, err
	}
	authz, err := authorization.NewCasbin(cfg.Rules)
	if err != nil {
		return nil, err
	}
	var authn authentication.Authenticator
	var evidence authentication.Evidence
	switch cfg.Mechanism {
	case "bearer":
		if cfg.JWT != nil {
			return nil, errors.New("bearer authentication accepts no JWT configuration")
		}
		if tokenEnv == "" {
			return nil, errors.New("bearer authentication requires --caller-token-env")
		}
		evidence.Token = os.Getenv(tokenEnv)
		tokens := make([]authentication.Token, len(cfg.Tokens))
		for i, t := range cfg.Tokens {
			hash, err := hex.DecodeString(t.SHA256)
			if err != nil {
				return nil, errors.New("invalid bearer digest")
			}
			tokens[i] = authentication.Token{Subject: t.Subject, Digest: hash}
		}
		authn, err = authentication.NewBearer(cfg.Issuer, tokens)
	case "jwt":
		if tokenEnv == "" || len(cfg.Tokens) > 0 {
			return nil, errors.New("JWT authentication requires --caller-token-env and accepts no static tokens")
		}
		evidence.Token = os.Getenv(tokenEnv)
		authn, err = configuredJWT(cfg.Issuer, cfg.JWT)
	case "os":
		if tokenEnv != "" || len(cfg.Tokens) > 0 || cfg.JWT != nil {
			return nil, errors.New("OS authentication accepts no bearer configuration")
		}
		authn, err = authentication.NewLocalOS(cfg.Issuer)
	default:
		return nil, errors.New("CLI access mechanism must be bearer, jwt or os")
	}
	if err != nil {
		return nil, err
	}
	identity, err := authn.Authenticate(ctx, evidence)
	if err != nil {
		return nil, err
	}
	session, err := authorization.NewSession(ctx, identity, directory, authz)
	if err != nil {
		return nil, err
	}
	return context.WithValue(session.Context(ctx), callerKey{}, session.Principal()), nil
}

// Only the authenticated directory snapshot is exposed; never infer an OS/cloud identity here.
type callerKey struct{}

func currentCaller(ctx context.Context) (principal.Principal, error) {
	caller, ok := ctx.Value(callerKey{}).(principal.Principal)
	if !ok {
		return nil, errors.New("no authenticated caller; supply --access-config")
	}
	return caller, nil
}
