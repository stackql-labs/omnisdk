package omnisdk

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Resource keys are logical identities within the same collection. Every run
// must use the same key for the same object and share the same state store.
type resourceLocks struct {
	mu     sync.Mutex
	h      *held
	users  map[string][]facade.LedgerKey
	writes map[string][]facade.LedgerKey
	done   map[string]bool
}

func graphLocks(g Graph, owners map[string]Managed, name string) *resourceLocks {
	l := &resourceLocks{users: map[string][]facade.LedgerKey{}, writes: map[string][]facade.LedgerKey{}, done: map[string]bool{}}
	parents := map[string][]string{}
	own := map[string][]facade.LedgerKey{}
	for _, n := range g.Nodes() {
		if m := owners[n.Alias()]; m != nil {
			own[n.Alias()] = append(own[n.Alias()], facade.LedgerKey(name+"/"+m.Key()))
			l.writes[n.Alias()] = append(l.writes[n.Alias()], facade.LedgerKey(name+"/"+m.Key()))
		}
		if in, ok := n.(intrinsicNode); ok && (n.Verb() == verbRecall || n.Verb() == verbDiff) {
			own[n.Alias()] = append(own[n.Alias()], facade.LedgerKey(name+"/"+in.key))
		}
		for _, p := range n.On() {
			for _, c := range predicateColumns(p) {
				if c.Qualifier() != n.Alias() {
					parents[n.Alias()] = append(parents[n.Alias()], c.Qualifier())
				}
			}
		}
	}
	for _, w := range g.Wirings() {
		for _, in := range w.Inbound() {
			parents[w.To()] = append(parents[w.To()], in.From())
		}
		for _, in := range wiringCandidates(w) {
			parents[w.To()] = append(parents[w.To()], in.From)
		}
	}
	for _, n := range g.Nodes() {
		seen := map[string]bool{}
		var walk func(string)
		walk = func(a string) {
			if seen[a] {
				return
			}
			seen[a] = true
			l.users[n.Alias()] = append(l.users[n.Alias()], own[a]...)
			// A committed managed child carries its own identity. Its consumers no
			// longer require exclusive access to the child's upstream resources.
			if a != n.Alias() && owners[a] != nil {
				return
			}
			for _, p := range parents[a] {
				walk(p)
			}
		}
		walk(n.Alias())
	}
	// Branches and global SQL expressions can consume data outside declared
	// wirings. Retain those footprints to the run boundary conservatively.
	if len(g.Branches()) > 0 || len(g.Filters()) > 0 || len(g.Projections()) > 0 {
		l.users["\x00run"] = l.keys()
		l.writes["\x00run"] = l.keys()
	}
	return l
}
func (l *resourceLocks) keys() []facade.LedgerKey {
	var keys []facade.LedgerKey
	for alias, ks := range l.users {
		if !l.done[alias] {
			keys = append(keys, ks...)
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}
func (l *resourceLocks) footprint() facade.KeySet {
	var writes []facade.LedgerKey
	for alias, keys := range l.writes {
		if !l.done[alias] {
			writes = append(writes, keys...)
		}
	}
	return lease.Using(writes, l.keys())
}
func (l *resourceLocks) complete(ctx context.Context, alias string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done[alias] {
		return nil
	}
	l.done[alias] = true
	return lease.Change(ctx, l.h.lease, l.footprint())
}
func (h *held) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.lease.Renew(ctx, leaseTTL)
}

func (h *held) maintain(parent context.Context, ttl time.Duration) {
	ctx, cancel := context.WithCancelCause(parent)
	h.ctx = ctx
	stopped := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := h.lease.Renew(ctx, ttl); err != nil {
					cancel(fmt.Errorf("omnisdk: write lease lost: %w", err))
					return
				}
			}
		}
	}()
	h.release = func() {
		close(quit)
		<-stopped
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = h.lease.Release(cleanup)
		cancel(nil)
	}
}

// Local access follows the same write/use rules within a run. Claim the whole
// footprint atomically, so a dependent waits for an active parent writer without
// retaining another lock that could deadlock that writer.
type localAccess struct {
	writer  bool
	readers int
}

func (r *managedRun) lockWriter(ctx context.Context, key facade.LedgerKey) (func(), error) {
	return r.lockMutation(ctx, key, nil)
}
func (r *managedRun) lockMutation(ctx context.Context, key facade.LedgerKey, dependencies []facade.LedgerKey) (func(), error) {
	deps := slices.Clone(dependencies)
	slices.Sort(deps)
	deps = slices.Compact(deps)
	deps = slices.DeleteFunc(deps, func(k facade.LedgerKey) bool { return k == key })
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.mu.Lock()
		if r.access == nil {
			r.access = map[facade.LedgerKey]localAccess{}
		}
		if r.accessChanged == nil {
			r.accessChanged = make(chan struct{})
		}
		own := r.access[key]
		blocked := own.writer || own.readers > 0
		for _, dep := range deps {
			if r.access[dep].writer {
				blocked = true
			}
		}
		if !blocked {
			r.access[key] = localAccess{writer: true}
			for _, dep := range deps {
				s := r.access[dep]
				s.readers++
				r.access[dep] = s
			}
			r.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					r.mu.Lock()
					defer r.mu.Unlock()
					delete(r.access, key)
					for _, dep := range deps {
						s := r.access[dep]
						s.readers--
						if s.readers == 0 {
							delete(r.access, dep)
						} else {
							r.access[dep] = s
						}
					}
					close(r.accessChanged)
					r.accessChanged = make(chan struct{})
				})
			}, nil
		}
		changed := r.accessChanged
		r.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
