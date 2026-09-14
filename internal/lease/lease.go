// Package lease implements the cross-run lock behind facade.Leaser.
//
// v1 is one lease over the whole collection: coarse but correct, and any finer scheme is a strict
// subset, so the refinement cannot break correctness later. Two things are built in now so that
// narrowing is a parameter rather than a rewrite — the lease takes a key set even though v1 only
// ever passes "all", and it expires rather than needing a manual unlock.
//
// It is stored as a single object naming the set, so conflict is set intersection and acquisition
// order never arises — which is the one place a genuine deadlock could appear.
package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// all is the unbounded set: v1's only caller passes it.
type all struct{}

// All returns the key set covering everything in scope.
func All() facade.KeySet { return all{} }

func (all) Contains(facade.LedgerKey) bool   { return true }
func (all) Intersects(facade.KeySet) bool    { return true }
func (all) Keys() ([]facade.LedgerKey, bool) { return nil, false }
func (all) String() string                   { return "*" }

type explicit struct{ keys []facade.LedgerKey }

// Of returns the key set naming exactly these keys — the shape a narrowed lease will use once a
// footprint can be computed. Unused by v1 and here so the interface is exercised.
func Of(keys ...facade.LedgerKey) facade.KeySet {
	k := slices.Clone(keys)
	slices.Sort(k)
	return explicit{keys: slices.Compact(k)}
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

// keyFor names the lease object for a scope. One object per scope, so v1's whole-collection lease
// is exactly one key.
func keyFor(scope string) facade.LedgerKey {
	return facade.LedgerKey(strings.TrimSuffix(scope, "/") + "/.lease")
}

func (l *leaser) Acquire(ctx context.Context, scope string, keys facade.KeySet, holder string, ttl time.Duration) (facade.Lease, error) {
	k := keyFor(scope)
	e, v, found, err := l.log.Get(ctx, k)
	if err != nil {
		return nil, fmt.Errorf("lease: read %s: %w", k, err)
	}
	if found {
		var held record
		if err := json.Unmarshal(e.Proposed(), &held); err != nil {
			return nil, fmt.Errorf("lease: decode %s: %w", k, err)
		}
		// A single object per scope holds one lease, so any live lease blocks. Testing
		// intersection is what narrowing will do; today the set is always everything.
		if l.now().Before(held.Expiry) {
			return nil, fmt.Errorf("%w by %s until %s", facade.ErrLeaseHeld, held.Holder, held.Expiry.Format(time.RFC3339))
		}
	} else {
		v = facade.LedgerVersionNone
	}
	return l.write(ctx, k, keys, holder, ttl, v)
}

func (l *leaser) write(ctx context.Context, k facade.LedgerKey, keys facade.KeySet, holder string, ttl time.Duration, v facade.LedgerVersion) (facade.Lease, error) {
	claim, err := newClaim()
	if err != nil {
		return nil, err
	}
	r := record{Claim: claim, Holder: holder, Expiry: l.now().Add(ttl)}
	if ks, bounded := keys.Keys(); bounded {
		r.Keys = make([]string, len(ks))
		for i, key := range ks {
			r.Keys[i] = string(key)
		}
	} else {
		r.All = true
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("lease: encode: %w", err)
	}
	if err := l.log.Begin(ctx, k, b, v); err != nil {
		return nil, fmt.Errorf("lease: claim %s: %w", k, err)
	}
	return &held{log: l.log, now: l.now, key: k, keys: keys, rec: r}, nil
}

// newClaim mints the per-acquisition identifier.
func newClaim() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("lease: claim id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

type held struct {
	log  facade.Ledger
	now  func() time.Time
	key  facade.LedgerKey
	keys facade.KeySet
	rec  record
}

func (h *held) Holder() string      { return h.rec.Holder }
func (h *held) Keys() facade.KeySet { return h.keys }
func (h *held) Expiry() time.Time   { return h.rec.Expiry }

// version re-reads the current version and confirms this holder still owns the lease. A lease that
// has been broken must not be renewed or released by its previous holder.
func (h *held) version(ctx context.Context) (facade.LedgerVersion, error) {
	e, v, found, err := h.log.Get(ctx, h.key)
	if err != nil {
		return "", fmt.Errorf("lease: read %s: %w", h.key, err)
	}
	if !found {
		return "", fmt.Errorf("%w: %s", facade.ErrLedgerNotFound, h.key)
	}
	var cur record
	if err := json.Unmarshal(e.Proposed(), &cur); err != nil {
		return "", fmt.Errorf("lease: decode %s: %w", h.key, err)
	}
	// Compared by claim, not holder: a re-acquisition invalidates the previous handle even when
	// the same runner took it again.
	if cur.Claim != h.rec.Claim {
		return "", fmt.Errorf("%w: %s now held by %s", facade.ErrLeaseHeld, h.key, cur.Holder)
	}
	return v, nil
}

func (h *held) Renew(ctx context.Context, ttl time.Duration) error {
	v, err := h.version(ctx)
	if err != nil {
		return err
	}
	next := h.rec
	next.Expiry = h.now().Add(ttl)
	b, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("lease: encode: %w", err)
	}
	if err := h.log.Begin(ctx, h.key, b, v); err != nil {
		return fmt.Errorf("lease: renew %s: %w", h.key, err)
	}
	h.rec = next
	return nil
}

func (h *held) Release(ctx context.Context) error {
	v, err := h.version(ctx)
	if err != nil {
		return err
	}
	if err := h.log.Forget(ctx, h.key, v); err != nil {
		return fmt.Errorf("lease: release %s: %w", h.key, err)
	}
	return nil
}
