package omnisdk

import (
	"context"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/rowcache"
	"strings"
)

type readCacheBridge struct{ cache rowcache.Cache }

func cacheKey(k facade.RowCacheKey) rowcache.Key {
	return rowcache.Key{Partition: k.Partition, Resource: k.Resource, Method: k.Method, Document: k.Document, Request: k.Request, Inputs: k.Inputs}
}
func (b readCacheBridge) Lookup(ctx context.Context, k facade.RowCacheKey) (facade.CachedRows, bool, error) {
	rows, hit, err := b.cache.Lookup(ctx, cacheKey(k))
	return rows, hit, err
}
func (b readCacheBridge) Begin(ctx context.Context, k facade.RowCacheKey) (facade.RowCacheWriter, error) {
	return b.cache.Begin(ctx, cacheKey(k))
}
func (b readCacheBridge) CacheError(err error) {
	if observer, ok := b.cache.(rowcache.Observer); ok {
		observer.CacheError(err)
	}
}
func appendReadCache(opts []docx.Option, args Args, address, provider string) []docx.Option {
	// An external page/row cap is not a completed API read. Keep it uncached.
	if !canCacheRead(args, provider) {
		return opts
	}
	return append(opts, docx.WithRowCache(readCacheBridge{args.ReadCache}, args.ReadCachePartition, address))
}

func canCacheRead(args Args, provider string) bool {
	if args.ReadCache == nil || args.Tuning.Limit > 0 {
		return false
	}
	pages := args.Tuning.PageLimit
	provider = strings.TrimPrefix(provider, "stackql_unstable_")
	override, ok := args.TuningByProvider["stackql_unstable_"+provider]
	if !ok {
		override = args.TuningByProvider[provider]
	}
	if override.PageLimit != 0 {
		pages = override.PageLimit
	}
	return pages <= 0
}
