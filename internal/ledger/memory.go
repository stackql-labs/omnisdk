// Package ledger implements the durable per-key log of resolved intent behind facade.Ledger.
//
// It depends on nothing but the standard library and facade's contracts: the executor calls the
// ledger, never the reverse. The in-memory implementation here is the reference for the semantics
// the durable ones must reproduce.
package ledger

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// entry is an immutable snapshot handed to callers; the store never shares a slice with them.
type entry struct {
	key      facade.LedgerKey
	phase    facade.LedgerPhase
	plan     []byte
	proposed []byte
	identity []byte
}

func (e entry) Key() facade.LedgerKey     { return e.key }
func (e entry) Phase() facade.LedgerPhase { return e.phase }
func (e entry) Plan() []byte              { return clone(e.plan) }
func (e entry) Proposed() []byte          { return clone(e.proposed) }
func (e entry) Identity() []byte          { return clone(e.identity) }

func sortKeys(k []facade.LedgerKey) { sort.Slice(k, func(i, j int) bool { return k[i] < k[j] }) }

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

type slot struct {
	e   entry
	ver uint64
}

type memory struct {
	mu   sync.Mutex
	rows map[facade.LedgerKey]*slot
}

// NewMemory returns a Ledger held entirely in memory. Suitable for tests and for a run that wants
// no durability; it satisfies every semantic the durable implementations must, including
// compare-and-swap, so a test written against it holds against them.
func NewMemory() facade.Ledger {
	return &memory{rows: make(map[facade.LedgerKey]*slot)}
}

func version(v uint64) facade.LedgerVersion { return facade.LedgerVersion(strconv.FormatUint(v, 10)) }

func (m *memory) Get(_ context.Context, k facade.LedgerKey) (facade.LedgerEntry, facade.LedgerVersion, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.rows[k]
	if !ok {
		return nil, facade.LedgerVersionNone, false, nil
	}
	return s.e, version(s.ver), true, nil
}

func (m *memory) List(_ context.Context, scope string) ([]facade.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]facade.LedgerEntry, 0, len(m.rows))
	for k, s := range m.rows {
		if strings.HasPrefix(string(k), scope) {
			out = append(out, s.e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

// check validates the presented version against the current state, returning the slot when the
// caller may proceed. LedgerVersionNone asserts absence, which is how a create is expressed.
func (m *memory) check(k facade.LedgerKey, v facade.LedgerVersion) (*slot, error) {
	s, ok := m.rows[k]
	switch {
	case v == facade.LedgerVersionNone && ok:
		return nil, fmt.Errorf("%w: %s exists", facade.ErrLedgerConflict, k)
	case v == facade.LedgerVersionNone:
		return nil, nil
	case !ok:
		return nil, fmt.Errorf("%w: %s", facade.ErrLedgerNotFound, k)
	case version(s.ver) != v:
		return nil, fmt.Errorf("%w: %s at %s, presented %s", facade.ErrLedgerConflict, k, version(s.ver), v)
	}
	return s, nil
}

func (m *memory) Begin(_ context.Context, k facade.LedgerKey, proposed []byte, v facade.LedgerVersion) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// A retried run presenting the same proposal must not conflict with itself, so an identical
	// pending entry is accepted whatever version the caller holds.
	if s, ok := m.rows[k]; ok && s.e.phase == facade.LedgerPending && bytes.Equal(s.e.proposed, proposed) {
		return nil
	}
	s, err := m.check(k, v)
	if err != nil {
		return err
	}
	if s == nil {
		m.rows[k] = &slot{e: entry{key: k, phase: facade.LedgerPending, proposed: clone(proposed)}, ver: 1}
		return nil
	}
	// Plan is left exactly as it was: losing n-1 to a call that may still fail is the one
	// unrecoverable mistake here.
	s.e.phase = facade.LedgerPending
	s.e.proposed = clone(proposed)
	s.ver++
	return nil
}

func (m *memory) Resolve(_ context.Context, k facade.LedgerKey, identity []byte, v facade.LedgerVersion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.check(k, v)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("%w: %s", facade.ErrLedgerNotFound, k)
	}
	if s.e.phase != facade.LedgerPending {
		return fmt.Errorf("%w: %s is live, expected pending", facade.ErrLedgerPhase, k)
	}
	s.e.plan = s.e.proposed
	s.e.proposed = nil
	s.e.identity = clone(identity)
	s.e.phase = facade.LedgerLive
	s.ver++
	return nil
}

func (m *memory) Forget(_ context.Context, k facade.LedgerKey, v facade.LedgerVersion) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.check(k, v)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("%w: %s", facade.ErrLedgerNotFound, k)
	}
	delete(m.rows, k)
	return nil
}
