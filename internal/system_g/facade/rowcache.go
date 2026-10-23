package facade

import "context"

// RowCacheKey identifies transformed API rows, independently of SQL projection.
// Partition is an explicit host-owned authorization/cache namespace.
type RowCacheKey struct {
	Partition, Resource, Method, Document, Request string
	Inputs                                         map[string]any
}

// RowCache is optional. Implementations are concurrent and honor cancellation.
// Lookup and write failures must not change the result of a live API read.
// A hit cursor failure is reported, never silently truncated.
type RowCache interface {
	Lookup(context.Context, RowCacheKey) (CachedRows, bool, error)
	Begin(context.Context, RowCacheKey) (RowCacheWriter, error)
}

type CachedRows interface {
	Next(context.Context) bool
	Row() map[string]any
	Err() error
	Close() error
}

// RowCacheWriter keeps writes invisible until Commit. Abort is idempotent.
type RowCacheWriter interface {
	Append(context.Context, map[string]any) error
	Commit(context.Context) error
	Abort(context.Context) error
}

// RowCacheErrorObserver receives best-effort cache diagnostics.
type RowCacheErrorObserver interface{ CacheError(error) }
