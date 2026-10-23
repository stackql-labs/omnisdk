package exchange_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/readcache"
	"github.com/stackql-labs/omnisdk/pkg/rowcache"
)

type cacheAdapter struct {
	rowcache.Cache
	errors atomic.Int32
}

func (a *cacheAdapter) Lookup(ctx context.Context, k facade.RowCacheKey) (facade.CachedRows, bool, error) {
	return a.Cache.Lookup(ctx, rowcache.Key{Partition: k.Partition, Resource: k.Resource, Method: k.Method, Document: k.Document, Request: k.Request})
}
func (a *cacheAdapter) Begin(ctx context.Context, k facade.RowCacheKey) (facade.RowCacheWriter, error) {
	return a.Cache.Begin(ctx, rowcache.Key{Partition: k.Partition, Resource: k.Resource, Method: k.Method, Document: k.Document, Request: k.Request})
}
func (a *cacheAdapter) CacheError(error) { a.errors.Add(1) }
func cacheFixture(t *testing.T) *cacheAdapter {
	t.Helper()
	c, err := rowcache.NewMemory(context.Background(), rowcache.Config{}, rowcache.MemoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &cacheAdapter{Cache: c}
}
func requestKey() facade.RowCacheKey {
	return facade.RowCacheKey{Partition: "tenant", Resource: "resource", Method: "list", Document: "doc", Request: "request"}
}

type cacheSource struct {
	opens      atomic.Int32
	rows       []map[string]any
	err        error
	incomplete bool
}

func (s *cacheSource) Open(ctx context.Context) facade.Records {
	s.opens.Add(1)
	return &cacheSourceRows{source: s, ctx: ctx}
}

type cacheSourceRows struct {
	source *cacheSource
	ctx    context.Context
	index  int
	row    facade.Record
	err    error
	closed bool
}

func (r *cacheSourceRows) Next(ctx context.Context) bool {
	if r.closed {
		return false
	}
	if err := ctx.Err(); err != nil {
		r.err = err
		return false
	}
	if r.index >= len(r.source.rows) {
		r.err = r.source.err
		if r.source.incomplete {
			readcache.MarkIncomplete(r.ctx)
		}
		return false
	}
	r.row = bind.NewDocRecord(r.source.rows[r.index])
	r.index++
	return true
}
func (r *cacheSourceRows) Record() facade.Record { return r.row }
func (r *cacheSourceRows) Err() error            { return r.err }
func (r *cacheSourceRows) Close() error          { r.closed = true; return nil }
func consume(op facade.Operator) (int, error) {
	rows := op.Open(context.Background())
	defer rows.Close()
	n := 0
	for rows.Next(context.Background()) {
		n++
	}
	return n, rows.Err()
}
func TestCachedRowsAbortsPartialFailedAndIgnoredReads(t *testing.T) {
	for _, scenario := range []string{"early-close", "failed", "ignored"} {
		t.Run(scenario, func(t *testing.T) {
			source := &cacheSource{rows: []map[string]any{{"id": 1}, {"id": 2}}}
			cache := cacheFixture(t)
			op := exchange.NewCachedRows(source, cache, requestKey())
			failure := errors.New("upstream interrupted")
			switch scenario {
			case "early-close":
				rows := op.Open(context.Background())
				if !rows.Next(context.Background()) {
					t.Fatal(rows.Err())
				}
				rows.Close()
			case "failed":
				source.err = failure
				if n, err := consume(op); n != 2 || !errors.Is(err, failure) {
					t.Fatalf("rows=%d err=%v", n, err)
				}
				source.err = nil
			case "ignored":
				source.incomplete = true
				if _, err := consume(op); err != nil {
					t.Fatal(err)
				}
				source.incomplete = false
			}
			if n, err := consume(op); n != 2 || err != nil {
				t.Fatalf("retry rows=%d err=%v", n, err)
			}
			if source.opens.Load() != 2 {
				t.Fatal("incomplete read published")
			}
			if n, err := consume(op); n != 2 || err != nil {
				t.Fatalf("hit rows=%d err=%v", n, err)
			}
			if source.opens.Load() != 2 {
				t.Fatal("complete read not reused")
			}
		})
	}
}
func TestCacheWriteFailureDoesNotChangeLiveRows(t *testing.T) {
	cache, err := rowcache.NewMemory(context.Background(), rowcache.Config{}, rowcache.MemoryConfig{MaxEntryBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	observer := &cacheAdapter{Cache: cache}
	source := &cacheSource{rows: []map[string]any{{"large": make([]byte, 2048)}}}
	op := exchange.NewCachedRows(source, observer, requestKey())
	for i := 0; i < 2; i++ {
		if n, err := consume(op); n != 1 || err != nil {
			t.Fatalf("rows=%d err=%v", n, err)
		}
	}
	if source.opens.Load() != 2 || observer.errors.Load() == 0 {
		t.Fatal("write failure was hidden or published")
	}
	cache.Close()
	if n, err := consume(op); n != 1 || err != nil {
		t.Fatal("closed cache broke live read", n, err)
	}
}

type blockingCache struct{ started chan struct{} }

func (c blockingCache) Lookup(ctx context.Context, _ facade.RowCacheKey) (facade.CachedRows, bool, error) {
	close(c.started)
	<-ctx.Done()
	return nil, false, ctx.Err()
}
func (c blockingCache) Begin(ctx context.Context, _ facade.RowCacheKey) (facade.RowCacheWriter, error) {
	return nil, ctx.Err()
}
func TestCacheLookupHonorsOperatorCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := blockingCache{started: make(chan struct{})}
	source := &cacheSource{}
	rows := exchange.NewCachedRows(source, cache, requestKey()).Open(parent)
	defer rows.Close()
	finished := make(chan struct{})
	go func() { rows.Next(context.Background()); close(finished) }()
	<-cache.started
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("cache lookup ignored operator cancellation")
	}
	if !errors.Is(rows.Err(), context.Canceled) {
		t.Fatal(rows.Err())
	}
}

type failingHit struct {
	err   error
	calls int
}

func (h *failingHit) Next(context.Context) bool { h.calls++; return h.calls == 1 }
func (h *failingHit) Row() map[string]any       { return map[string]any{"id": 1} }
func (h *failingHit) Err() error                { return h.err }
func (h *failingHit) Close() error              { return nil }

type brokenHitCache struct{ hit *failingHit }

func (c brokenHitCache) Lookup(context.Context, facade.RowCacheKey) (facade.CachedRows, bool, error) {
	return c.hit, true, nil
}
func (c brokenHitCache) Begin(context.Context, facade.RowCacheKey) (facade.RowCacheWriter, error) {
	return nil, errors.New("unexpected write")
}
func TestCacheHitFailureDoesNotTruncateOrDuplicateRows(t *testing.T) {
	failure := errors.New("cache cursor failed")
	source := &cacheSource{rows: []map[string]any{{"id": 1}, {"id": 2}}}
	n, err := consume(exchange.NewCachedRows(source, brokenHitCache{&failingHit{err: failure}}, requestKey()))
	if n != 1 || !errors.Is(err, failure) || source.opens.Load() != 0 {
		t.Fatalf("rows=%d err=%v opens=%d", n, err, source.opens.Load())
	}
}

func TestDisabledCacheAddsNoWrapperOrAllocation(t *testing.T) {
	source := &cacheSource{}
	if exchange.NewCachedRows(source, nil, facade.RowCacheKey{}) != source {
		t.Fatal("disabled cache wrapped operator")
	}
	if allocations := testing.AllocsPerRun(100, func() {
		_ = exchange.NewCachedRows(source, nil, facade.RowCacheKey{})
	}); allocations != 0 {
		t.Fatalf("disabled cache allocated %v", allocations)
	}
}

type retainedContextHit struct {
	ctx   context.Context
	index int
	err   error
}

func (h *retainedContextHit) Next(context.Context) bool {
	if err := h.ctx.Err(); err != nil {
		h.err = err
		return false
	}
	h.index++
	return h.index <= 2
}
func (h *retainedContextHit) Row() map[string]any { return map[string]any{"id": h.index} }
func (h *retainedContextHit) Err() error          { return h.err }
func (h *retainedContextHit) Close() error        { return nil }

type retainedContextCache struct{}

func (retainedContextCache) Lookup(ctx context.Context, _ facade.RowCacheKey) (facade.CachedRows, bool, error) {
	return &retainedContextHit{ctx: ctx}, true, nil
}
func (retainedContextCache) Begin(context.Context, facade.RowCacheKey) (facade.RowCacheWriter, error) {
	return nil, errors.New("unexpected write")
}
func TestCacheHitContextSurvivesMultiplePulls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	op := exchange.NewCachedRows(&cacheSource{}, retainedContextCache{}, requestKey())
	rows := op.Open(ctx)
	defer rows.Close()
	for i := 0; i < 2; i++ {
		if !rows.Next(ctx) {
			t.Fatalf("pull %d: %v", i, rows.Err())
		}
	}
	if rows.Next(ctx) || rows.Err() != nil {
		t.Fatal(rows.Err())
	}
}
