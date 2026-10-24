// Package readcache carries read-completion provenance, without a storage dependency.
package readcache

import (
	"context"
	"sync/atomic"
)

type key struct{}
type Attempt interface{ Failed() bool }
type attempt struct{ failed atomic.Bool }

func (a *attempt) Failed() bool { return a.failed.Load() }
func WithAttempt(ctx context.Context) (context.Context, Attempt) {
	a := &attempt{}
	return context.WithValue(ctx, key{}, a), a
}

// MarkIncomplete prevents an ignored HTTP failure from becoming a cache entry.
func MarkIncomplete(ctx context.Context) {
	if a, ok := ctx.Value(key{}).(*attempt); ok {
		a.failed.Store(true)
	}
}
