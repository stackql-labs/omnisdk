// Package cache is a bounded, cost-weighted LRU cache.
//
// Its budget is a total cost, not an entry count, because what it holds varies by orders of
// magnitude: one parsed provider document can outweigh a hundred others. The budget may be stated,
// or derived from the memory the process is allowed, so a deployment sized for more memory caches
// more without configuration. Standard library only.
package cache

import (
	"container/list"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cache holds values under keys, evicting the least recently used when over budget.
type Cache[K comparable, V any] interface {
	// Get returns the value for k, marking it recently used. An expired entry is absent.
	Get(k K) (V, bool)
	// Put stores v under k at the given cost, evicting as needed. A value costing more than the
	// whole budget is not stored.
	Put(k K, v V, cost int64)
	// Len is the number of entries; Cost is their total cost.
	Len() int
	Cost() int64
}

// Config bounds a cache. The zero Config is usable: an automatic budget, no entry limit, no expiry.
type Config struct {
	// MaxCost is the total cost held. Zero derives it from the process's memory limit — Fraction of
	// GOMEMLIMIT, else of the container's cgroup limit, else Fallback.
	MaxCost int64
	// Fraction is the share of the memory limit an automatic budget takes. Zero means 0.25.
	Fraction float64
	// Fallback is the automatic budget where no memory limit is known. Zero means 512 MiB.
	Fallback int64
	// MaxEntries caps the number of entries as well. Zero is no cap.
	MaxEntries int
	// TTL expires an entry this long after it was stored. Zero never expires.
	TTL time.Duration
}

// Budget is the total cost a Config allows, resolving an automatic one.
func (c Config) Budget() int64 {
	if c.MaxCost > 0 {
		return c.MaxCost
	}
	frac := c.Fraction
	if frac <= 0 || frac > 1 {
		frac = 0.25
	}
	if limit := memoryLimit(); limit > 0 {
		return int64(float64(limit) * frac)
	}
	if c.Fallback > 0 {
		return c.Fallback
	}
	return 512 << 20
}

// memoryLimit is what the process may use: GOMEMLIMIT where set, else a cgroup v2 or v1 limit, else 0.
func memoryLimit() int64 {
	if l := debug.SetMemoryLimit(-1); l > 0 && l < math.MaxInt64 {
		return l
	}
	for _, p := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 && n < 1<<60 {
			return n
		}
	}
	return 0
}

// New builds a cache bounded by cfg.
func New[K comparable, V any](cfg Config) Cache[K, V] {
	return &lru[K, V]{cfg: cfg, budget: cfg.Budget(), order: list.New(), items: map[K]*list.Element{}, now: time.Now}
}

type entry[K comparable, V any] struct {
	key    K
	value  V
	cost   int64
	stored time.Time
}

type lru[K comparable, V any] struct {
	mu     sync.Mutex
	cfg    Config
	budget int64
	cost   int64
	order  *list.List // front is most recently used
	items  map[K]*list.Element
	now    func() time.Time
}

func (c *lru[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[k]
	if !ok {
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if c.cfg.TTL > 0 && c.now().Sub(e.stored) > c.cfg.TTL {
		c.remove(el)
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.value, true
}

func (c *lru[K, V]) Put(k K, v V, cost int64) {
	if cost < 0 {
		cost = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.remove(el)
	}
	if cost > c.budget {
		return
	}
	c.items[k] = c.order.PushFront(&entry[K, V]{key: k, value: v, cost: cost, stored: c.now()})
	c.cost += cost
	for c.cost > c.budget || (c.cfg.MaxEntries > 0 && c.order.Len() > c.cfg.MaxEntries) {
		c.remove(c.order.Back())
	}
}

func (c *lru[K, V]) remove(el *list.Element) {
	e := el.Value.(*entry[K, V])
	c.order.Remove(el)
	delete(c.items, e.key)
	c.cost -= e.cost
}

func (c *lru[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *lru[K, V]) Cost() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cost
}
