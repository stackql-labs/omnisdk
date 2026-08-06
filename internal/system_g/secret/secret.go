// Package secret resolves a configuration value (a credential, key, or secret string) from
// wherever it is supplied — a direct literal, an env var, or a file — so env vars are a
// convenience, never a requirement. Callers depend on the resolved value, not its origin.
package secret

import (
	"fmt"
	"os"
	"strings"
)

// Source yields a string value, or an error if it was configured but unreadable.
type Source interface {
	Resolve() (string, error)
}

type literal struct{ v string }

// Literal is a value passed in directly (e.g. a flag).
func Literal(v string) Source { return literal{v} }

func (l literal) Resolve() (string, error) { return l.v, nil }

type env struct{ name string }

// Env reads the named environment variable ("" if unset).
func Env(name string) Source { return env{name} }

func (e env) Resolve() (string, error) { return os.Getenv(e.name), nil }

type file struct{ path string }

// File reads the file's contents ("" if the path is empty; error if set but unreadable).
func File(path string) Source { return file{path} }

func (f file) Resolve() (string, error) {
	if strings.TrimSpace(f.path) == "" {
		return "", nil
	}
	b, err := os.ReadFile(f.path)
	if err != nil {
		return "", fmt.Errorf("secret: read %s: %w", f.path, err)
	}
	return string(b), nil
}

type firstNonEmpty struct {
	label string
	srcs  []Source
}

// Require yields the first source with a non-empty value; it errors (naming label) if none do or
// a source is broken. Use for mandatory values.
func Require(label string, srcs ...Source) Source { return firstNonEmpty{label: label, srcs: srcs} }

func (f firstNonEmpty) Resolve() (string, error) {
	for _, s := range f.srcs {
		v, err := s.Resolve()
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(v) != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("secret: %s not provided (pass it directly or set the env var)", f.label)
}

// Optional yields the first non-empty value, or "" if none; broken sources are skipped. Use for
// values with a sensible absence (e.g. a session token) or a trailing Literal default.
func Optional(srcs ...Source) string {
	for _, s := range srcs {
		if v, err := s.Resolve(); err == nil && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
