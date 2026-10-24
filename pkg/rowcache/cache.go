// Package rowcache provides an opt-in, transactional cache of transformed API
// rows. Storage and expiry collection are independent of query execution. The
// package depends only on the standard library and knows nothing about SQL,
// provider SDKs, HTTP, authentication, or System G.
package rowcache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrClosed      = errors.New("rowcache: closed")
	ErrCapacity    = errors.New("rowcache: capacity exceeded")
	ErrUnsupported = errors.New("rowcache: unsupported row value")
)

// Key identifies a complete API read. Partition must be supplied explicitly by
// the host and separate authorization contexts. Inputs are request bindings,
// never query projections. Implementations must not mutate them.
type Key struct {
	Partition string         `json:"partition"`
	Resource  string         `json:"resource"`
	Method    string         `json:"method"`
	Document  string         `json:"document"`
	Request   string         `json:"request"`
	Inputs    map[string]any `json:"inputs,omitempty"`
}

type Rows interface {
	Next(context.Context) bool
	Row() map[string]any
	Err() error
	Close() error
}

type Writer interface {
	Append(context.Context, map[string]any) error
	Commit(context.Context) error
	Abort(context.Context) error
}

type Cache interface {
	Lookup(context.Context, Key) (Rows, bool, error)
	Begin(context.Context, Key) (Writer, error)
}

// Store owns atomic publication and physical expiry. Lookup must treat an
// expired entry as a miss, even when Collect has not run. A failed Commit must
// leave the old entry intact. Each writer/cursor is used serially; stores are
// concurrent. All methods must honor cancellation. New does not own the store.
type Store interface {
	Lookup(context.Context, Key, time.Time) (Rows, bool, error)
	Begin(context.Context, Key) (Transaction, error)
	Collect(context.Context, time.Time) (int, error)
}

type Transaction interface {
	Append(context.Context, map[string]any) error
	Commit(context.Context, time.Time) error
	Abort(context.Context) error
}

type Observer interface{ CacheError(error) }

// Config sets expiry and collection cadence. Zero uses a five-minute TTL and
// a thirty-second collector interval. Observer must be fast and concurrency safe.
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Observer   Observer
}

// Managed owns only its collector. Close cancels and joins that worker; the
// caller still owns an external Store. Cancellation of New's context also stops
// collection and disables new operations. No query starts a collector.
type Managed interface {
	Cache
	Observer
	Collect(context.Context) (int, error)
	Close() error
}

type managed struct {
	store   Store
	cfg     Config
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
	cleanup func()
}

func New(ctx context.Context, store Store, cfg Config) (Managed, error) {
	return newManaged(ctx, store, cfg, nil)
}
func newManaged(ctx context.Context, store Store, cfg Config, cleanup func()) (Managed, error) {
	if ctx == nil || store == nil {
		return nil, fmt.Errorf("rowcache: context and store are required")
	}
	if cfg.TTL < 0 || cfg.GCInterval < 0 {
		return nil, fmt.Errorf("rowcache: TTL and GC interval must be positive")
	}
	if cfg.TTL == 0 {
		cfg.TTL = 5 * time.Minute
	}
	if cfg.GCInterval == 0 {
		cfg.GCInterval = 30 * time.Second
	}
	lifetime, cancel := context.WithCancel(ctx)
	c := &managed{store: store, cfg: cfg, ctx: lifetime, cancel: cancel, done: make(chan struct{}), cleanup: cleanup}
	go c.collector()
	return c, nil
}
func (c *managed) collector() {
	defer close(c.done)
	if c.cleanup != nil {
		defer c.cleanup()
	}
	ticker := time.NewTicker(c.cfg.GCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			_, err := c.store.Collect(c.ctx, time.Now())
			if err != nil && c.ctx.Err() == nil {
				c.CacheError(err)
			}
		}
	}
}
func (c *managed) Close() error { c.once.Do(c.cancel); <-c.done; return nil }
func (c *managed) CacheError(err error) {
	if err != nil && c.cfg.Observer != nil {
		c.cfg.Observer.CacheError(err)
	}
}
func (c *managed) active(ctx context.Context, key Key) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.ctx.Err() != nil {
		return ErrClosed
	}
	if key.Partition == "" || key.Resource == "" || key.Method == "" || key.Document == "" || key.Request == "" {
		return fmt.Errorf("rowcache: partition, resource, method, document and request are required")
	}
	return nil
}
func (c *managed) Lookup(ctx context.Context, key Key) (Rows, bool, error) {
	if err := c.active(ctx, key); err != nil {
		return nil, false, err
	}
	return c.store.Lookup(ctx, key, time.Now())
}
func (c *managed) Begin(ctx context.Context, key Key) (Writer, error) {
	if err := c.active(ctx, key); err != nil {
		return nil, err
	}
	tx, err := c.store.Begin(ctx, key)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, fmt.Errorf("rowcache: store returned a nil transaction")
	}
	return &writer{owner: c, tx: tx}, nil
}
func (c *managed) Collect(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if c.ctx.Err() != nil {
		return 0, ErrClosed
	}
	return c.store.Collect(ctx, time.Now())
}

type writer struct {
	owner    *managed
	tx       Transaction
	finished bool
}

func (w *writer) Append(ctx context.Context, row map[string]any) error {
	if w.finished || w.owner.ctx.Err() != nil {
		return ErrClosed
	}
	return w.tx.Append(ctx, row)
}
func (w *writer) Commit(ctx context.Context) error {
	if w.finished {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.owner.ctx.Err() != nil {
		return ErrClosed
	}
	err := w.tx.Commit(ctx, time.Now().Add(w.owner.cfg.TTL))
	if err == nil {
		w.finished = true
	}
	return err
}
func (w *writer) Abort(ctx context.Context) error {
	if w.finished {
		return nil
	}
	err := w.tx.Abort(ctx)
	if err == nil {
		w.finished = true
	}
	return err
}
