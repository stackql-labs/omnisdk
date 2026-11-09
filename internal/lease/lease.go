// Package lease implements the cross-run lock behind facade.Leaser.
//
// Each scope stores one CAS-updated registry of claims. Disjoint resource sets
// can run concurrently; overlapping sets and collection-wide claims conflict.
// Footprints are acquired atomically and narrowed as resources reach their
// last consumer, without acquiring individual keys in a deadlock-prone order.
package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// all is the unbounded set used by collection scans and destruction.
type all struct{}

// All returns the key set covering everything in scope.
func All() facade.KeySet { return all{} }

func (all) Contains(facade.LedgerKey) bool   { return true }
func (all) Intersects(facade.KeySet) bool    { return true }
func (all) Keys() ([]facade.LedgerKey, bool) { return nil, false }
func (all) String() string                   { return "*" }

type explicit struct{ keys []facade.LedgerKey }

// Of returns the exact resource footprint of a managed graph.
func Of(keys ...facade.LedgerKey) facade.KeySet {
	k := slices.Clone(keys)
	slices.Sort(k)
	return explicit{keys: slices.Compact(k)}
}

// Using claims exclusive access to writes and shared protection for reads.
// Shared guards prevent mutation/deletion while allowing independent consumers.
func Using(writes, reads []facade.LedgerKey) facade.KeySet {
	allKeys := append(slices.Clone(writes), reads...)
	exclusive := Of(writes...)
	shared := slices.DeleteFunc(slices.Clone(reads), exclusive.Contains)
	return accessSet{KeySet: Of(allKeys...), shared: Of(shared...)}
}

type accessSet struct {
	facade.KeySet
	shared facade.KeySet
}

func sharedKeys(keys facade.KeySet) facade.KeySet {
	if a, ok := keys.(accessSet); ok {
		return a.shared
	}
	return Of()
}
func conflicts(a, b facade.KeySet) bool {
	ak, ab := a.Keys()
	bk, bb := b.Keys()
	if !ab {
		return !bb || len(bk) > 0
	}
	if !bb {
		return len(ak) > 0
	}
	ar, br := sharedKeys(a), sharedKeys(b)
	for _, key := range ak {
		if b.Contains(key) && !(ar.Contains(key) && br.Contains(key)) {
			return true
		}
	}
	return false
}

func (e explicit) Contains(k facade.LedgerKey) bool { return slices.Contains(e.keys, k) }

func (e explicit) Intersects(other facade.KeySet) bool {
	ks, bounded := other.Keys()
	if !bounded {
		return true
	}
	return slices.ContainsFunc(ks, e.Contains)
}

func (e explicit) Keys() ([]facade.LedgerKey, bool) { return slices.Clone(e.keys), true }

func (e explicit) String() string {
	parts := make([]string, len(e.keys))
	for i, k := range e.keys {
		parts[i] = string(k)
	}
	return strings.Join(parts, ",")
}

// record is the durable lease. Holder and expiry are what let a lease left by a dead process be
// broken without a manual unlock.
//
// Claim is unique per acquisition, and is what makes a claim an event rather than a value: without
// it two runners sharing a holder name would write identical bytes, and the ledger — which treats
// an identical proposal as a retry of the same intent — would admit both.
type record struct {
	Claim  string    `json:"claim"`
	Holder string    `json:"holder"`
	Expiry time.Time `json:"expiry"`
	Keys   []string  `json:"keys,omitempty"`
	Shared []string  `json:"shared_keys,omitempty"`
	All    bool      `json:"all,omitempty"`
}

// leaser stores leases as entries in a Ledger, reusing its compare-and-swap rather than repeating
// it. A lease is recorded as an entry that is never resolved: it is intent that does not commit —
// it is released.
type leaser struct {
	log facade.Ledger
	now func() time.Time
}

// NewLeaser returns a Leaser over the given ledger. now is injectable so expiry is testable
// without sleeping.
func NewLeaser(log facade.Ledger, now func() time.Time) facade.Leaser {
	if now == nil {
		now = time.Now
	}
	return &leaser{log: log, now: now}
}

// keyFor names the CAS registry for a scope.
func keyFor(scope string) facade.LedgerKey {
	return facade.LedgerKey(strings.TrimSuffix(scope, "/") + "/.lease")
}

// registry is updated with one CAS, so overlapping key sets cannot both win.
// Empty registries are retained to avoid reusing a version after release.
type registry struct {
	Generation string   `json:"generation"`
	Claims     []record `json:"claims"`
}

func read(ctx context.Context, log facade.Ledger, key facade.LedgerKey) (registry, facade.LedgerVersion, error) {
	e, v, found, err := log.Get(ctx, key)
	if err != nil {
		return registry{}, v, err
	}
	if !found {
		return registry{}, facade.LedgerVersionNone, nil
	}
	var r registry
	if err := json.Unmarshal(e.Proposed(), &r); err != nil {
		return r, v, err
	}
	if r.Generation == "" { // Upgrade a legacy collection-wide lease without breaking it.
		var old record
		if err := json.Unmarshal(e.Proposed(), &old); err != nil {
			return r, v, err
		}
		if old.Claim == "" {
			return r, v, fmt.Errorf("lease: invalid registry %s", key)
		}
		r.Claims = []record{old}
	}
	return r, v, nil
}

func keysOf(r record) facade.KeySet {
	if r.All {
		return All()
	}
	keys := make([]facade.LedgerKey, len(r.Keys))
	for i, k := range r.Keys {
		keys[i] = facade.LedgerKey(k)
	}
	reads := make([]facade.LedgerKey, len(r.Shared))
	for i, k := range r.Shared {
		reads[i] = facade.LedgerKey(k)
	}
	return accessSet{KeySet: Of(keys...), shared: Of(reads...)}
}

func setKeys(r *record, keys facade.KeySet) {
	r.Keys = nil
	r.Shared = nil
	shared, _ := sharedKeys(keys).Keys()
	for _, k := range shared {
		r.Shared = append(r.Shared, string(k))
	}
	ks, bounded := keys.Keys()
	r.All = !bounded
	for _, k := range ks {
		r.Keys = append(r.Keys, string(k))
	}
}

func mutate(ctx context.Context, log facade.Ledger, key facade.LedgerKey, change func(*registry) error) error {
	for attempts := 0; attempts < 128; attempts++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, v, err := read(ctx, log, key)
		if err != nil {
			return err
		}
		if err := change(&r); err != nil {
			return err
		}
		r.Generation, err = newClaim()
		if err != nil {
			return err
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		err = log.Begin(ctx, key, b, v)
		if errors.Is(err, facade.ErrLedgerConflict) {
			continue
		}
		return err
	}
	return fmt.Errorf("lease: registry contention: %w", facade.ErrLedgerConflict)
}

func (l *leaser) Acquire(ctx context.Context, scope string, keys facade.KeySet, holder string, ttl time.Duration) (facade.Lease, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("lease: ttl must be positive")
	}
	claim, err := newClaim()
	if err != nil {
		return nil, err
	}
	rec := record{Claim: claim, Holder: holder, Expiry: l.now().Add(ttl)}
	setKeys(&rec, keys)
	key := keyFor(scope)
	err = mutate(ctx, l.log, key, func(r *registry) error {
		r.Claims = slices.DeleteFunc(r.Claims, func(c record) bool { return !l.now().Before(c.Expiry) })
		for _, c := range r.Claims {
			if conflicts(keys, keysOf(c)) {
				return fmt.Errorf("%w by %s until %s", facade.ErrLeaseHeld, c.Holder, c.Expiry)
			}
		}
		r.Claims = append(r.Claims, rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &held{log: l.log, now: l.now, key: key, rec: rec}, nil
}

func newClaim() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type held struct {
	mu  sync.Mutex
	log facade.Ledger
	now func() time.Time
	key facade.LedgerKey
	rec record
}

func (h *held) Holder() string      { h.mu.Lock(); defer h.mu.Unlock(); return h.rec.Holder }
func (h *held) Keys() facade.KeySet { h.mu.Lock(); defer h.mu.Unlock(); return keysOf(h.rec) }
func (h *held) Expiry() time.Time   { h.mu.Lock(); defer h.mu.Unlock(); return h.rec.Expiry }

// Change atomically replaces a held footprint. Shrinking releases terminal resources;
// expansion is used before compensation and must conflict with every other writer.
func Change(ctx context.Context, l facade.Lease, keys facade.KeySet) error {
	h, ok := l.(*held)
	if !ok {
		return fmt.Errorf("lease: unsupported lease implementation")
	}
	return h.change(ctx, keys, 0, false)
}
func (h *held) change(ctx context.Context, keys facade.KeySet, ttl time.Duration, release bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := h.rec
	if keys != nil {
		setKeys(&next, keys)
	}
	if ttl > 0 {
		next.Expiry = h.now().Add(ttl)
	}
	err := mutate(ctx, h.log, h.key, func(r *registry) error {
		index := slices.IndexFunc(r.Claims, func(c record) bool { return c.Claim == h.rec.Claim })
		if index < 0 || !h.now().Before(r.Claims[index].Expiry) {
			return fmt.Errorf("%w: lease lost or expired", facade.ErrLeaseHeld)
		}
		if release {
			r.Claims = slices.Delete(r.Claims, index, index+1)
			return nil
		}
		for _, c := range r.Claims {
			if c.Claim != next.Claim && h.now().Before(c.Expiry) && conflicts(keysOf(next), keysOf(c)) {
				return facade.ErrLeaseHeld
			}
		}
		r.Claims[index] = next
		return nil
	})
	if err == nil {
		h.rec = next
	}
	return err
}
func (h *held) Renew(ctx context.Context, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("lease: ttl must be positive")
	}
	return h.change(ctx, nil, ttl, false)
}
func (h *held) Release(ctx context.Context) error { return h.change(ctx, nil, 0, true) }
