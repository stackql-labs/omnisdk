package rowcache

import (
	"container/heap"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// MemoryConfig bounds retained entries AND pending writes. Costs include row
// payload and estimated container overhead, not exact process RSS. Zero uses
// 64 MiB total, 4 MiB per entry, and 1024 entries/pending writers.
type MemoryConfig struct {
	MaxBytes      int64
	MaxEntryBytes int64
	MaxEntries    int
}

// NewMemory creates an independently collected, bounded in-memory cache. Close
// or parent cancellation releases its entries. Existing readers own snapshots.
func NewMemory(ctx context.Context, cfg Config, limits MemoryConfig) (Managed, error) {
	if limits.MaxBytes < 0 || limits.MaxEntryBytes < 0 || limits.MaxEntries < 0 {
		return nil, fmt.Errorf("rowcache: negative memory limit")
	}
	if limits.MaxBytes == 0 {
		limits.MaxBytes = 64 << 20
	}
	if limits.MaxEntryBytes == 0 {
		limits.MaxEntryBytes = 4 << 20
	}
	if limits.MaxEntries == 0 {
		limits.MaxEntries = 1024
	}
	if limits.MaxEntryBytes > limits.MaxBytes {
		limits.MaxEntryBytes = limits.MaxBytes
	}
	s := &memory{limits: limits, entries: map[[32]byte]*entry{}, order: list.New()}
	return newManaged(ctx, s, cfg, s.close)
}

type memory struct {
	mu              sync.Mutex
	limits          MemoryConfig
	entries         map[[32]byte]*entry
	expiry          expiryHeap
	order           *list.List
	bytes, reserved int64
	writers         int
	closed          bool
}
type entry struct {
	key     [32]byte
	rows    []map[string]any
	cost    int64
	expires time.Time
	index   int
	lru     *list.Element
}
type expiryHeap []*entry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].expires.Before(h[j].expires) }
func (h expiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].index = i; h[j].index = j }
func (h *expiryHeap) Push(v any)        { e := v.(*entry); e.index = len(*h); *h = append(*h, e) }
func (h *expiryHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	old[len(old)-1] = nil
	e.index = -1
	*h = old[:len(old)-1]
	return e
}
func digest(key Key) ([32]byte, error) {
	b, err := json.Marshal(key)
	if err != nil {
		return [32]byte{}, fmt.Errorf("rowcache key: %w", err)
	}
	return sha256.Sum256(b), nil
}
func (s *memory) remove(e *entry) {
	delete(s.entries, e.key)
	s.order.Remove(e.lru)
	heap.Remove(&s.expiry, e.index)
	s.bytes -= e.cost
}
func (s *memory) reserve(cost int64, protected [32]byte) bool {
	for s.bytes+s.reserved+cost > s.limits.MaxBytes {
		candidate := s.order.Back()
		for candidate != nil && candidate.Value.(*entry).key == protected {
			candidate = candidate.Prev()
		}
		if candidate == nil {
			return false
		}
		s.remove(candidate.Value.(*entry))
	}
	if s.bytes+s.reserved+cost > s.limits.MaxBytes {
		return false
	}
	s.reserved += cost
	return true
}
func (s *memory) Lookup(ctx context.Context, key Key, now time.Time) (Rows, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	id, err := digest(key)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, ErrClosed
	}
	e := s.entries[id]
	if e == nil {
		return nil, false, nil
	}
	if !now.Before(e.expires) {
		s.remove(e)
		return nil, false, nil
	}
	s.order.MoveToFront(e.lru)
	return &memoryRows{rows: e.rows}, true, nil
}
func (s *memory) Begin(ctx context.Context, key Key) (Transaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := digest(key)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	const initialCost = 128
	if s.writers >= s.limits.MaxEntries || initialCost > s.limits.MaxEntryBytes || !s.reserve(initialCost, id) {
		return nil, ErrCapacity
	}
	s.writers++
	return &memoryWriter{store: s, key: id, cost: initialCost}, nil
}
func (s *memory) Collect(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	n := 0
	for len(s.expiry) > 0 && !now.Before(s.expiry[0].expires) {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		s.remove(s.expiry[0])
		n++
	}
	return n, nil
}
func (s *memory) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.entries = nil
	s.expiry = nil
	s.order.Init()
	s.bytes = 0
}

type memoryWriter struct {
	store    *memory
	key      [32]byte
	rows     []map[string]any
	cost     int64
	finished bool
}

func (w *memoryWriter) Append(ctx context.Context, row map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.finished {
		return ErrClosed
	}
	remaining := w.store.limits.MaxEntryBytes - w.cost
	cost, err := valueCost(row, 0, remaining)
	if err != nil {
		return err
	}
	// Reserve before allocating the copy; concurrent writers share one budget.
	s := w.store
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	if !s.reserve(cost, w.key) {
		s.mu.Unlock()
		return ErrCapacity
	}
	s.mu.Unlock()
	w.cost += cost
	w.rows = append(w.rows, cloneValue(row).(map[string]any))
	return nil
}
func (w *memoryWriter) Commit(ctx context.Context, expires time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.finished {
		return ErrClosed
	}
	s := w.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if old := s.entries[w.key]; old != nil {
		s.remove(old)
	}
	for len(s.entries) >= s.limits.MaxEntries {
		s.remove(s.order.Back().Value.(*entry))
	}
	e := &entry{key: w.key, rows: w.rows, cost: w.cost, expires: expires}
	e.lru = s.order.PushFront(e)
	heap.Push(&s.expiry, e)
	s.entries[w.key] = e
	s.reserved -= w.cost
	s.bytes += w.cost
	s.writers--
	w.finished = true
	w.rows = nil
	return nil
}
func (w *memoryWriter) Abort(_ context.Context) error {
	if w.finished {
		return nil
	}
	s := w.store
	s.mu.Lock()
	s.reserved -= w.cost
	s.writers--
	s.mu.Unlock()
	w.finished = true
	w.rows = nil
	return nil
}

type memoryRows struct {
	rows    []map[string]any
	index   int
	current map[string]any
	err     error
	closed  bool
}

func (r *memoryRows) Next(ctx context.Context) bool {
	if r.closed || r.err != nil {
		return false
	}
	if err := ctx.Err(); err != nil {
		r.err = err
		return false
	}
	if r.index >= len(r.rows) {
		r.current = nil
		return false
	}
	r.current = cloneValue(r.rows[r.index]).(map[string]any)
	r.index++
	return true
}
func (r *memoryRows) Row() map[string]any { return r.current }
func (r *memoryRows) Err() error          { return r.err }
func (r *memoryRows) Close() error        { r.closed = true; r.rows = nil; r.current = nil; return nil }

// Bounded traversal rejects unsupported/cyclic values before allocating a copy.
func valueCost(v any, depth int, budget int64) (int64, error) {
	if depth > 64 {
		return 0, ErrUnsupported
	}
	cost := int64(32)
	switch v := v.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
	case string:
		cost += int64(len(v))
	case json.Number:
		cost += int64(len(v))
	case []byte:
		cost += int64(len(v))
	case map[string]any:
		cost += int64(len(v)) * 80
		if cost > budget {
			return 0, ErrCapacity
		}
		for name, value := range v {
			cost += int64(len(name))
			n, err := valueCost(value, depth+1, budget-cost)
			if err != nil {
				return 0, err
			}
			cost += n
		}
	case []any:
		cost += int64(len(v)) * 16
		if cost > budget {
			return 0, ErrCapacity
		}
		for _, value := range v {
			n, err := valueCost(value, depth+1, budget-cost)
			if err != nil {
				return 0, err
			}
			cost += n
		}
	default:
		return 0, ErrUnsupported
	}
	if cost > budget {
		return 0, ErrCapacity
	}
	return cost, nil
}
func cloneValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		if v == nil {
			return map[string]any(nil)
		}
		out := make(map[string]any, len(v))
		for k, value := range v {
			out[k] = cloneValue(value)
		}
		return out
	case []any:
		if v == nil {
			return []any(nil)
		}
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = cloneValue(value)
		}
		return out
	case []byte:
		return append([]byte(nil), v...)
	default:
		return v
	}
}
