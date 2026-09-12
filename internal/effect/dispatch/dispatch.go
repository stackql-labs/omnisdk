// Package dispatch routes effects to the provider that owns the key.
//
// A collection is a set of keys a run manages together, and in general those span providers — a VPC
// at AWS and a DNS record elsewhere belong to one deployment. Each key's address names its provider,
// so routing is a prefix match and nothing above it needs to know which providers are involved.
package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type router struct{ routes map[string]facade.Effector }

// New returns an Effector that routes by key prefix. Prefixes are matched longest-first, so a
// provider may register a whole service and another register one resource beneath it.
func New(routes map[string]facade.Effector) facade.Effector {
	return &router{routes: routes}
}

// forKey finds the most specific registered prefix for a key. The collection name is the first
// segment and is not part of the address, so matching starts after it.
func (r *router) forKey(k facade.LedgerKey) (facade.Effector, error) {
	_, addr, ok := strings.Cut(string(k), "/")
	if !ok {
		return nil, fmt.Errorf("dispatch: key %q has no address after the collection name", k)
	}
	best := ""
	for prefix := range r.routes {
		if strings.HasPrefix(addr, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return nil, fmt.Errorf("dispatch: no provider registered for %q", addr)
	}
	return r.routes[best], nil
}

func (r *router) Effect(ctx context.Context, exchange string, k facade.LedgerKey, in facade.EffectInput) ([]byte, error) {
	e, err := r.forKey(k)
	if err != nil {
		return nil, err
	}
	return e.Effect(ctx, exchange, k, in)
}

func (r *router) Read(ctx context.Context, exchange string, k facade.LedgerKey, in facade.EffectInput) ([]byte, []byte, bool, error) {
	e, err := r.forKey(k)
	if err != nil {
		return nil, nil, false, err
	}
	return e.Read(ctx, exchange, k, in)
}
