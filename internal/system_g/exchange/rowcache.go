package exchange

import (
	"context"
	"fmt"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/readcache"
)

// NewCachedRows attaches an optional cache before projection or input echoing.
// Lookup/write failures leave live reads intact; a failing hit cursor surfaces
// its error rather than silently truncating or replaying already emitted rows.
func NewCachedRows(source facade.Operator, cache facade.RowCache, key facade.RowCacheKey) facade.Operator {
	if cache == nil {
		return source
	}
	return cachedOperator{source: source, cache: cache, key: key}
}

type cachedOperator struct {
	source facade.Operator
	cache  facade.RowCache
	key    facade.RowCacheKey
}

func (o cachedOperator) Open(ctx context.Context) facade.Records {
	lifetime, cancel := context.WithCancel(ctx)
	return &cachedRecords{op: o, ctx: lifetime, parent: ctx, cancel: cancel}
}

type cachedRecords struct {
	parent                context.Context
	cancel                context.CancelFunc
	op                    cachedOperator
	ctx                   context.Context
	source                facade.Records
	hit                   facade.CachedRows
	writer                facade.RowCacheWriter
	attempt               readcache.Attempt
	current               facade.Record
	err                   error
	started, done, closed bool
}

func (r *cachedRecords) report(err error) {
	if err != nil {
		if observer, ok := r.op.cache.(facade.RowCacheErrorObserver); ok {
			observer.CacheError(err)
		}
	}
}
func (r *cachedRecords) abort() {
	if r.writer == nil {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), time.Second)
	defer cancel()
	r.report(r.writer.Abort(cleanup))
	r.writer = nil
}
func (r *cachedRecords) start(ctx context.Context) {
	r.started = true
	hit, present, err := r.op.cache.Lookup(ctx, r.op.key)
	if err == nil && present && hit != nil {
		r.hit = hit
		return
	}
	if hit != nil {
		r.report(hit.Close())
	}
	if present && hit == nil && err == nil {
		err = fmt.Errorf("row cache returned a nil hit cursor")
	}
	r.report(err)
	writer, err := r.op.cache.Begin(ctx, r.op.key)
	if err == nil {
		r.writer = writer
	} else {
		r.report(err)
		if writer != nil {
			r.report(writer.Abort(context.WithoutCancel(ctx)))
		}
	}
	sourceCtx, attempt := readcache.WithAttempt(r.ctx)
	r.attempt = attempt
	r.source = r.op.source.Open(sourceCtx)
}
func (r *cachedRecords) Next(ctx context.Context) bool {
	// A backend cursor/transaction may retain the context supplied to Lookup or
	// Begin. Keep that context alive until Close, rather than cancelling it after
	// the first row. Independent per-pull cancellation terminates this cursor.
	if ctx.Done() != nil && ctx.Done() != r.parent.Done() {
		stop := context.AfterFunc(ctx, r.cancel)
		defer stop()
	}
	if r.closed || r.done {
		return false
	}
	if err := ctx.Err(); err != nil {
		r.err = err
		r.done = true
		r.abort()
		return false
	}
	if err := r.ctx.Err(); err != nil {
		r.err = err
		r.done = true
		r.abort()
		return false
	}
	ctx = r.ctx
	if !r.started {
		r.start(ctx)
	}
	if r.hit != nil {
		if r.hit.Next(ctx) {
			r.current = bind.NewDocRecord(r.hit.Row())
			return true
		}
		r.err = r.hit.Err()
		r.report(r.err)
		r.done = true
		r.report(r.hit.Close())
		r.hit = nil
		return false
	}
	if r.source.Next(ctx) {
		r.current = r.source.Record()
		if r.writer != nil {
			doc, ok := r.current.Doc(facade.AnonymousPayload)
			row, object := doc.(map[string]any)
			if !ok || !object {
				r.report(fmt.Errorf("row cache requires object rows"))
				r.abort()
			} else if err := r.writer.Append(ctx, row); err != nil {
				r.report(err)
				r.abort()
			}
		}
		return true
	}
	r.done = true
	r.err = r.source.Err()
	if r.err == nil {
		r.err = ctx.Err()
	}
	if r.err == nil {
		r.err = r.ctx.Err()
	}
	if r.err != nil || r.attempt.Failed() {
		r.abort()
		return false
	}
	if r.writer != nil {
		if err := r.writer.Commit(ctx); err != nil {
			r.report(err)
			r.abort()
		} else {
			r.writer = nil
		}
	}
	return false
}
func (r *cachedRecords) Record() facade.Record { return r.current }
func (r *cachedRecords) Err() error            { return r.err }
func (r *cachedRecords) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.cancel()
	r.abort()
	r.current = nil
	if r.hit != nil {
		return r.hit.Close()
	}
	if r.source != nil {
		return r.source.Close()
	}
	return nil
}
