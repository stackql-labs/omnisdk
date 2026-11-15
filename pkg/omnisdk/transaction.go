package omnisdk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/public/authorization"
)

// TransactionHandle is an opaque session-bound capability, not authentication.
// ID is a durable locator suitable for an explicitly authorized resume.
type TransactionHandle struct {
	id         string
	generation string
	session    *TransactionSession
}

func (h TransactionHandle) ID() string { return h.id }

// TransactionSession owns transaction capabilities for one host session.
// Hosts must allocate one per SQL connection, never share it between connections.
type TransactionSession struct {
	mu     sync.Mutex
	caller authorization.Session
	active map[string]*transaction
}

// TransactionConfig names the existing IaC coordination namespace. Resource keys
// are logical keys in that collection, not provider addresses or transaction IDs.
type TransactionTarget struct {
	Key         string `json:"key"`
	Identity    string `json:"identity"`
	AddressedBy string `json:"addressed_by"`
}

type TransactionMode string

const (
	TransactionEager TransactionMode = "eager"
	TransactionLazy  TransactionMode = "lazy"
)

type TransactionConfig struct {
	LeaseSeconds   int                          `json:"lease_seconds,omitempty"`
	Mode           TransactionMode              `json:"mode,omitempty"`
	TimeoutSeconds int                          `json:"timeout_seconds,omitempty"`
	Targets        map[string]TransactionTarget `json:"targets,omitempty"`
	Collection     string                       `json:"collection"`
	State          string                       `json:"state"`
	Keys           []string                     `json:"keys"`
	DynamicScope   bool                         `json:"dynamic_scope,omitempty"`
}

type transactionEffect struct {
	InverseMethod  string               `json:"inverse_method,omitempty"`
	Scope          map[string]string    `json:"scope,omitempty"`
	Key            string               `json:"key"`
	Address        string               `json:"address"`
	Verb           string               `json:"verb"`
	Identity       string               `json:"identity"`
	IdentityColumn string               `json:"identity_column"`
	AddressedBy    string               `json:"addressed_by"`
	Version        facade.LedgerVersion `json:"version"`
}

type transactionIntent struct {
	Key            string                  `json:"key"`
	Address        string                  `json:"address"`
	IdentityColumn string                  `json:"identity_column"`
	AddressedBy    string                  `json:"addressed_by"`
	InverseMethod  string                  `json:"inverse_method"`
	Tokens         map[string]InverseToken `json:"tokens"`
	Scope          map[string]string       `json:"scope"`
	BeforeVersion  facade.LedgerVersion    `json:"before_version"`
	BeforePresent  bool                    `json:"before_present"`
}

type transactionRecord struct {
	Attachment     string                       `json:"attachment"`
	LeaseSeconds   int                          `json:"lease_seconds,omitempty"`
	InFlight       []transactionIntent          `json:"in_flight,omitempty"`
	SchemaVersion  int                          `json:"schema_version"`
	Mode           TransactionMode              `json:"mode"`
	Endpoint       string                       `json:"endpoint,omitempty"`
	EndpointBound  bool                         `json:"endpoint_bound,omitempty"`
	TimeoutSeconds int                          `json:"timeout_seconds,omitempty"`
	Documents      map[string]string            `json:"documents,omitempty"`
	Queue          []json.RawMessage            `json:"queue,omitempty"`
	Running        bool                         `json:"running,omitempty"`
	Targets        map[string]TransactionTarget `json:"targets,omitempty"`
	Effects        []transactionEffect          `json:"effects,omitempty"`
	DynamicScope   bool                         `json:"dynamic_scope,omitempty"`
	Owner          string                       `json:"owner"`
	Status         string                       `json:"status"`
	Keys           []string                     `json:"keys"`
	Journal        string                       `json:"journal"`
}
type transactionManaged struct {
	Managed
	inverse DeclaredInverse
}

func (m transactionManaged) transactionInverse() DeclaredInverse { return m.inverse }

type transactionWorkerKey struct{}

type transaction struct {
	version      facade.LedgerVersion
	recoveryOnly bool
	generation   string
	readers      int
	config       TransactionConfig
	record       transactionRecord
	held         *held
	run          *managedRun
	failed       error
	draining     bool
}

// NewTransactionSession follows host authorization: nil leaves auth disabled.
// Configured callers must have a resolved identity; handles remain session-bound.
func NewTransactionSession(caller authorization.Session) (*TransactionSession, error) {
	if caller != nil && (caller.Principal() == nil || caller.Principal().ID() == "") {
		return nil, fmt.Errorf("%w: transaction session requires an authenticated caller", authorization.ErrDenied)
	}
	return &TransactionSession{caller: caller, active: map[string]*transaction{}}, nil
}
func (s *TransactionSession) owner() string {
	if s.caller == nil {
		return ""
	}
	return s.caller.Principal().ID()
}
func (s *TransactionSession) authorize(ctx context.Context, collection, action string) (context.Context, error) {
	permission := facade.Permission{Resource: "collection/" + collection, Action: action}
	if principal := facade.AccessPrincipal(ctx); principal != nil {
		if principal.ID() != s.owner() {
			return nil, authorization.ErrDenied
		}
		return facade.Authorize(ctx, permission)
	}
	if facade.AccessEnabled(ctx) {
		return nil, authorization.ErrDenied
	}
	return authorizeExecution(ctx, Args{Caller: s.caller, RequireCaller: s.caller != nil}, permission)
}
func transactionKey(collection, id string) facade.LedgerKey {
	return facade.LedgerKey(collection + "/.transactions/" + id)
}
func transactionKeys(config TransactionConfig, id string) (facade.KeySet, error) {
	if config.Collection == "" || config.State == "" || strings.ContainsAny(config.Collection, "/\\\x00") || strings.HasPrefix(config.Collection, ".") {
		return nil, fmt.Errorf("transaction requires a collection and state directory")
	}
	keys := []facade.LedgerKey{transactionKey(config.Collection, id)}
	for _, key := range config.Keys {
		if key == "" || strings.ContainsAny(key, "\\\x00") {
			return nil, fmt.Errorf("invalid transaction resource key")
		}
		for _, part := range strings.Split(key, "/") {
			if part == "" || strings.HasPrefix(part, ".") {
				return nil, fmt.Errorf("invalid transaction resource key")
			}
		}
		keys = append(keys, facade.LedgerKey(config.Collection+"/"+key))
	}
	return lease.Of(keys...), nil
}
func saveTransaction(ctx context.Context, t *transaction, id string) error {
	if err := t.held.check(ctx); err != nil {
		return err
	}
	key := transactionKey(t.config.Collection, id)
	_, version, found, err := t.held.log.Get(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		version = facade.LedgerVersionNone
	}
	if version != t.version {
		return facade.ErrLedgerConflict
	}
	t.run.mu.Lock()
	t.record.Effects = nil
	for _, e := range t.run.applied {
		t.record.Effects = append(t.record.Effects, transactionEffect{Key: string(e.key), Address: e.address, Verb: e.verb, Identity: e.identity, IdentityColumn: e.resource.Identity(), AddressedBy: e.resource.AddressedBy(), Version: e.version, InverseMethod: e.inverseMethod, Scope: maps.Clone(e.inverseParams)})
	}
	t.run.mu.Unlock()
	raw, err := json.Marshal(t.record)
	if err != nil {
		return err
	}
	if err = t.held.log.Begin(ctx, key, raw, version); err != nil {
		return err
	}
	_, version, _, err = t.held.log.Get(ctx, key)
	if err != nil {
		return err
	}
	t.version = version
	if err = t.held.log.Resolve(ctx, key, []byte(id), nil, version); err != nil {
		return err
	}
	_, version, _, err = t.held.log.Get(ctx, key)
	if err != nil {
		return err
	}
	t.version = version
	return nil
}

// Begin acquires the complete declared write footprint before any mutation and
// persists ownership/status in the same ledger used by IaC. No desired-state
// comparison is performed. The session must Finish or Close the transaction.
func (s *TransactionSession) Begin(ctx context.Context, config TransactionConfig) (TransactionHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if config.Mode == "" {
		config.Mode = TransactionEager
	}
	if config.Mode != TransactionEager && config.Mode != TransactionLazy {
		return TransactionHandle{}, fmt.Errorf("transaction mode must be eager or lazy")
	}
	id := rand.Text()
	keys, err := transactionKeys(config, id)
	if err != nil {
		return TransactionHandle{}, err
	}
	ctx, err = s.authorize(ctx, config.Collection, "transaction.begin")
	if err != nil {
		return TransactionHandle{}, err
	}
	// A statement request ending does not end its SQL session. Close owns cleanup.
	lifetime := config.TimeoutSeconds
	if lifetime == 0 {
		lifetime = 900
	}
	if lifetime < 1 {
		return TransactionHandle{}, fmt.Errorf("transaction timeout must be positive")
	}
	config.TimeoutSeconds = lifetime
	lifetimeCtx, stopLifetime := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(lifetime)*time.Second)
	if config.LeaseSeconds == 0 {
		config.LeaseSeconds = int(leaseTTL / time.Second)
	}
	if config.LeaseSeconds < 1 {
		stopLifetime()
		return TransactionHandle{}, fmt.Errorf("lease duration must be positive")
	}
	h, err := holdWithTTL(lifetimeCtx, config.State, config.Collection, "txn-"+id, time.Duration(config.LeaseSeconds)*time.Second, keys)
	if err != nil {
		stopLifetime()
		return TransactionHandle{}, err
	}
	release := h.release
	h.release = func() { release(); stopLifetime() }
	config.Keys = append([]string(nil), config.Keys...)
	config.Targets = maps.Clone(config.Targets)
	t := &transaction{generation: rand.Text(), config: config, held: h, record: transactionRecord{Attachment: h.lease.Holder(), SchemaVersion: 1, LeaseSeconds: config.LeaseSeconds, Mode: config.Mode, Owner: s.owner(), Status: "active", TimeoutSeconds: config.TimeoutSeconds, Keys: config.Keys, DynamicScope: config.DynamicScope, Targets: config.Targets, Journal: "txn-" + id}}
	t.run = &managedRun{name: config.Collection, ledger: h.log, journal: h.journal, held: h}
	if err = saveTransaction(ctx, t, id); err != nil {
		h.release()
		return TransactionHandle{}, err
	}
	s.active[id] = t
	return TransactionHandle{id: id, generation: t.generation, session: s}, nil
}
func (s *TransactionSession) lookup(handle TransactionHandle) (*transaction, error) {
	if handle.session != s || handle.id == "" {
		return nil, fmt.Errorf("%w: foreign transaction handle", authorization.ErrDenied)
	}
	t := s.active[handle.id]
	if t != nil && t.generation != handle.generation {
		return nil, fmt.Errorf("%w: stale transaction capability", authorization.ErrDenied)
	}
	if t == nil {
		return nil, fmt.Errorf("transaction handle is closed")
	}
	return t, nil
}

// Execute runs an already resolved query graph using IaC's managed mutation
// wrapper, journal, ledger and retained leases. Scope is declared at Begin or
// extended atomically in dynamic mode. Until update/delete inversion is available, those forms are
// rejected before dispatch, rather than advertised as rollback-safe.
func (s *TransactionSession) Execute(ctx context.Context, handle TransactionHandle, registry string, g Graph, managed []Managed, args Args) (result Rows, retErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.lookup(handle)
	if err != nil {
		return nil, err
	}
	if t.readers > 0 {
		return nil, fmt.Errorf("transaction read cursor is still open")
	}
	if t.record.Running && ctx.Value(transactionWorkerKey{}) != t {
		return nil, fmt.Errorf("transaction is executing another statement")
	}
	ctx, err = s.authorize(ctx, t.config.Collection, "transaction.execute")
	if err != nil {
		return nil, err
	}
	if t.failed != nil {
		return nil, fmt.Errorf("transaction is aborted; rollback required: %w", t.failed)
	}
	if err = t.held.check(ctx); err != nil {
		t.failed = err
		return nil, err
	}
	var implicitLocks []string
	if len(managed) == 0 && len(t.config.Targets) == 0 && t.config.DynamicScope {
		managed, implicitLocks, err = implicitTransactionManaged(registry, g, handle.id, len(t.record.Effects), args)
		if err != nil {
			return nil, err
		}
	}
	if t.config.DynamicScope {
		expanded := t.config
		expanded.Keys = append(append([]string(nil), expanded.Keys...), implicitLocks...)
		for _, m := range managed {
			if !lease.Exclusive(t.held.lease.Keys(), t.run.keyOf(m.Key())) {
				expanded.Keys = append(expanded.Keys, m.Key())
			}
		}
		footprint, scopeErr := transactionKeys(expanded, handle.id)
		if scopeErr != nil {
			return nil, scopeErr
		}
		if scopeErr = lease.Change(ctx, t.held.lease, footprint); scopeErr != nil {
			t.failed = scopeErr
			return nil, scopeErr
		}
		t.config = expanded
		t.record.Keys = expanded.Keys
	}
	defer func() {
		if retErr != nil && t.failed == nil {
			t.failed = retErr
			t.record.Status = "aborted"
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if persistErr := saveTransaction(cleanup, t, handle.id); persistErr != nil {
				retErr = errors.Join(retErr, persistErr)
			}
		}
	}()
	nodes := map[string]Managed{}
	for _, m := range managed {
		if !lease.Exclusive(t.held.lease.Keys(), t.run.keyOf(m.Key())) {
			return nil, fmt.Errorf("resource %s is outside transaction scope", m.Key())
		}
		if m.Identity() == "" || m.AddressedBy() == "" {
			return nil, fmt.Errorf("transaction mutation requires identity and inverse addressing")
		}
		for _, alias := range m.Aliases() {
			nodes[alias] = m
		}
	}
	for _, n := range g.Nodes() {
		if n.Verb() != "select" && n.Verb() != "insert" {
			return nil, fmt.Errorf("transaction has no verified inverse for %s", n.Verb())
		}
		if n.Verb() == "insert" {
			if _, _, binding, bindingErr := args.binding(n.Address()); bindingErr != nil {
				return nil, bindingErr
			} else if binding != nil {
				return nil, fmt.Errorf("transactional provider aliases require durable tenancy bindings; use canonical addresses")
			}
			if len(t.config.Targets) > 0 {
				declared, found := t.config.Targets[n.Address()]
				m := nodes[n.Alias()]
				if !found || m == nil || declared.Key != m.Key() || declared.Identity != m.Identity() || declared.AddressedBy != m.AddressedBy() {
					return nil, fmt.Errorf("mutation does not match the durable transaction target declaration")
				}
			}
			if nodes[n.Alias()] == nil {
				return nil, fmt.Errorf("transaction mutation %s has no managed target", n.Alias())
			}
			if _, err = DescribeMutation(registry, n.Address(), "delete", args.documentOptions()...); err != nil {
				return nil, fmt.Errorf("transaction create has no delete inverse: %w", err)
			}
			declaration, inverseErr := DocInverse(registry, n.Address(), "insert", args.documentOptions()...)
			if inverseErr != nil {
				return nil, inverseErr
			}
			if declaration == nil {
				return nil, fmt.Errorf("transaction mutation %s has no declared inverse", n.Address())
			}
			if _, err = s.authorize(ctx, t.config.Collection, "transaction.rollback"); err != nil {
				return nil, err
			}
			ctx, err = authorizeInverse(ctx, registry, n.Address(), declaration, args)
			if err != nil {
				return nil, err
			}
			for name, token := range declaration.Tokens() {
				if token.Location != "body" || token.Algorithm != "toString" || token.Key == "" {
					return nil, fmt.Errorf("unsupported inverse token %s", name)
				}
			}
			nodes[n.Alias()] = transactionManaged{Managed: nodes[n.Alias()], inverse: declaration}
			fingerprint, fingerprintErr := inverseDocument(registry, n.Address(), args)
			if fingerprintErr != nil {
				return nil, fingerprintErr
			}
			if t.record.Documents == nil {
				t.record.Documents = map[string]string{}
			}
			if prior := t.record.Documents[n.Address()]; prior != "" && prior != fingerprint {
				return nil, fmt.Errorf("transaction inverse document changed")
			}
			t.record.Documents[n.Address()] = fingerprint
		}
	}
	args.Caller = nil
	args.ReadCache = nil
	args.RequireCaller = facade.AccessEnabled(ctx) || args.RequireCaller
	args.TxnRollback = true
	args.IACRollback = false
	args.Journal = nil
	// Validate ownership and graph structure through the existing managed planner.
	if _, err = ConvergeGraph(registry, t.config.Collection, t.config.State, "probe", g, managed, args); err != nil {
		return nil, err
	}
	t.run.nodes = nodes
	args.managed = t.run
	plan, err := NewGraphQuery(registry, g, args)
	if err != nil {
		return nil, err
	}
	// Persist the uncertain interval before the managed wrapper can send anything.
	if t.record.EndpointBound && t.record.Endpoint != args.Endpoint {
		return nil, fmt.Errorf("transaction endpoint changed")
	}
	t.record.Endpoint = args.Endpoint
	t.record.EndpointBound = true
	t.record.InFlight = nil
	for _, n := range g.Nodes() {
		m := nodes[n.Alias()]
		if m == nil {
			continue
		}
		declaration := m.(transactionManaged).inverse
		_, beforeVersion, beforePresent, readErr := t.held.log.Get(ctx, t.run.keyOf(m.Key()))
		if readErr != nil {
			return nil, readErr
		}
		scope := map[string]string{}
		captureInverseScope(scope, args.Params)
		captureInverseScope(scope, n.Params())
		t.record.InFlight = append(t.record.InFlight, transactionIntent{Key: m.Key(), Address: n.Address(), IdentityColumn: m.Identity(), AddressedBy: m.AddressedBy(), InverseMethod: declaration.Method(), Tokens: declaration.Tokens(), Scope: scope, BeforeVersion: beforeVersion, BeforePresent: beforePresent})
	}
	t.record.Status = "executing"
	if err = saveTransaction(ctx, t, handle.id); err != nil {
		t.failed = err
		return nil, err
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(t.held.ctx, func() { cancel(context.Cause(t.held.ctx)) })
	defer func() { stop(); cancel(nil) }()
	t.run.mu.Lock()
	appliedBefore := len(t.run.applied)
	t.run.mu.Unlock()
	rows, err := plan.Open(runCtx)
	var output []Row
	if err == nil {
		for rows.Next() {
			output = append(output, rows.Row())
		}
		err = rows.Err()
		_ = rows.Close()
	}
	if err == nil {
		err = runCtx.Err()
	}
	t.run.mu.Lock()
	for i := appliedBefore; i < len(t.run.applied); i++ {
		e := &t.run.applied[i]
		if e.inverseParams == nil {
			e.inverseParams = map[string]string{}
		}
		captureInverseScope(e.inverseParams, args.Params)
		for _, node := range g.Nodes() {
			if node.Address() == e.address {
				captureInverseScope(e.inverseParams, node.Params())
			}
		}
	}
	t.run.mu.Unlock()
	var uncertain []transactionIntent
	for _, intent := range t.record.InFlight {
		entry, version, found, readErr := t.held.log.Get(ctx, t.run.keyOf(intent.Key))
		known := readErr == nil && ((!found && !intent.BeforePresent) || (found && version == intent.BeforeVersion))
		if found && entry.Phase() == facade.LedgerPending {
			known = false
		}
		if found && entry.Phase() == facade.LedgerLive {
			for _, effect := range t.run.applied {
				if effect.key == t.run.keyOf(intent.Key) && effect.version == version {
					known = true
				}
			}
		}
		if !known {
			uncertain = append(uncertain, intent)
		}
	}
	t.record.InFlight = uncertain
	if err != nil {
		t.failed = err
		t.record.Status = "aborted"
	} else {
		t.record.Status = "active"
	}
	if persistErr := saveTransaction(ctx, t, handle.id); persistErr != nil {
		t.failed = errors.Join(err, persistErr)
		return nil, t.failed
	}
	if err != nil {
		return nil, err
	}
	return &sliceRows{rows: output, i: -1}, nil
}

// Commit publishes a durable terminal status before releasing retained locks.
func (s *TransactionSession) Commit(ctx context.Context, handle TransactionHandle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.lookup(handle)
	if err != nil {
		return err
	}
	if t.readers > 0 {
		return fmt.Errorf("transaction read cursor is still open")
	}
	ctx, err = s.authorize(ctx, t.config.Collection, "transaction.commit")
	if err != nil {
		return err
	}
	if t.failed != nil {
		return fmt.Errorf("transaction is aborted; rollback required: %w", t.failed)
	}
	if err = t.held.check(ctx); err != nil {
		return err
	}
	if _, err = beginRollback(ctx, t.held, t.config.Collection, t.record.Journal); err != nil {
		return err
	}
	if len(t.record.Queue) > 0 || t.record.Running {
		return fmt.Errorf("transaction has deferred statements; drain before commit")
	}
	t.record.Status = "committed"
	if err = saveTransaction(ctx, t, handle.id); err != nil {
		t.failed = err
		return err
	}
	t.held.release()
	delete(s.active, handle.id)
	return nil
}

// Rollback uses the existing managed compensation path under the original locks.
// Uncertain outcomes or incomplete inverses remain explicit, never successful.
func (s *TransactionSession) Rollback(ctx context.Context, handle TransactionHandle, registry string, args Args) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.lookup(handle)
	if err != nil {
		return err
	}
	if t.record.Running {
		return fmt.Errorf("transaction statement is still running")
	}
	if t.readers > 0 {
		return fmt.Errorf("transaction read cursor is still open")
	}
	ctx, err = s.authorize(ctx, t.config.Collection, "transaction.rollback")
	if err != nil {
		return err
	}
	if len(t.record.InFlight) > 0 || t.recoveryOnly {
		return fmt.Errorf("transaction has unresolved statement outcomes; recover before rollback")
	}
	if _, err = beginRollback(ctx, t.held, t.config.Collection, t.record.Journal); err != nil {
		return err
	}
	args.Caller = nil
	args.RequireCaller = facade.AccessEnabled(ctx) || args.RequireCaller
	args.TxnRollback = true
	args.Journal = nil
	if t.record.EndpointBound && t.record.Endpoint != args.Endpoint {
		return fmt.Errorf("transaction inverse endpoint changed")
	}
	for address, expected := range t.record.Documents {
		current, fingerprintErr := inverseDocument(registry, address, args)
		if fingerprintErr != nil {
			return fingerprintErr
		}
		if current != expected {
			return fmt.Errorf("transaction inverse document changed: %s", address)
		}
	}
	plan := convergeGraphPlan{registry: registry, args: args}
	_, left := plan.compensate(ctx, t.run)
	if len(left) > 0 {
		return fmt.Errorf("transaction compensation outstanding: %v", left)
	}
	t.record.Queue = nil
	t.record.Running = false
	t.record.Status = "rolled_back"
	if err = saveTransaction(ctx, t, handle.id); err != nil {
		return err
	}
	t.held.release()
	delete(s.active, handle.id)
	return nil
}

// Close abandons live handles without claiming rollback. Durable resource intent
// and transaction status remain available for explicit authenticated recovery.
func (s *TransactionSession) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.active {
		t.held.release()
		delete(s.active, id)
	}
}

// InspectTransaction is a protected durable status read. It exposes no capability
// and does not acquire execution rights or replay effects.
func InspectTransaction(ctx context.Context, config TransactionConfig, id string, args Args) (string, error) {
	if _, err := transactionKeys(config, id); err != nil {
		return "", err
	}
	if id == "" || strings.ContainsAny(id, "/\\.\x00") {
		return "", fmt.Errorf("invalid transaction ID")
	}
	ctx, err := authorizeExecution(ctx, args, facade.Permission{Resource: "collection/" + config.Collection, Action: "transaction.inspect"})
	if err != nil {
		return "", err
	}
	if facade.AccessEnabled(ctx) && facade.AccessPrincipal(ctx) == nil {
		return "", authorization.ErrDenied
	}
	log, err := ledger.NewFile(filepath.Join(config.State, "ledger"))
	if err != nil {
		return "", err
	}
	entry, _, found, err := log.Get(ctx, transactionKey(config.Collection, id))
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("transaction not found")
	}
	if entry.Phase() != facade.LedgerLive {
		return "unresolved", nil
	}
	var record transactionRecord
	if err = json.Unmarshal(entry.Plan(), &record); err != nil {
		return "", err
	}
	return record.Status, nil
}

// Resume creates a new capability after fresh authentication and authorization.
// It reacquires the recorded footprint; it never replays a possibly sent request.
func (s *TransactionSession) Resume(ctx context.Context, config TransactionConfig, id string) (TransactionHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" || strings.ContainsAny(id, "/\\.\x00") {
		return TransactionHandle{}, fmt.Errorf("invalid transaction ID")
	}
	if _, err := transactionKeys(config, id); err != nil {
		return TransactionHandle{}, err
	}
	ctx, err := s.authorize(ctx, config.Collection, "transaction.resume")
	if err != nil {
		return TransactionHandle{}, err
	}
	log, err := ledger.NewFile(filepath.Join(config.State, "ledger"))
	if err != nil {
		return TransactionHandle{}, err
	}
	key := transactionKey(config.Collection, id)
	entry, version, found, err := log.Get(ctx, key)
	if err != nil {
		return TransactionHandle{}, err
	}
	if !found {
		return TransactionHandle{}, fmt.Errorf("transaction not found")
	}
	pendingStatus := entry.Phase() != facade.LedgerLive
	rawStatus := entry.Plan()
	if pendingStatus {
		rawStatus = entry.Proposed()
	}
	var record transactionRecord
	if err = json.Unmarshal(rawStatus, &record); err != nil {
		return TransactionHandle{}, err
	}
	if record.SchemaVersion != 1 {
		return TransactionHandle{}, fmt.Errorf("unsupported transaction schema version")
	}
	if record.Owner != s.owner() {
		return TransactionHandle{}, fmt.Errorf("%w: transaction owner differs", authorization.ErrDenied)
	}
	if record.Status != "active" && record.Status != "aborted" && record.Status != "executing" && !(pendingStatus && (record.Status == "committed" || record.Status == "rolled_back")) {
		return TransactionHandle{}, fmt.Errorf("transaction %s cannot resume: %s", id, record.Status)
	}
	config.Keys = record.Keys
	config.Targets = record.Targets
	config.TimeoutSeconds = record.TimeoutSeconds
	config.LeaseSeconds = record.LeaseSeconds
	if config.LeaseSeconds == 0 {
		config.LeaseSeconds = int(leaseTTL / time.Second)
	}
	config.Mode = record.Mode
	config.DynamicScope = record.DynamicScope
	keys, err := transactionKeys(config, id)
	if err != nil {
		return TransactionHandle{}, err
	}
	l, err := lease.NewLeaser(log, nil).Acquire(ctx, config.Collection, keys, "resume-"+rand.Text(), time.Duration(config.LeaseSeconds)*time.Second)
	if err != nil {
		return TransactionHandle{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = l.Release(context.Background())
		}
	}()
	_, current, _, err := log.Get(ctx, key)
	if err != nil {
		return TransactionHandle{}, err
	}
	if current != version {
		return TransactionHandle{}, facade.ErrLedgerConflict
	}
	journals, err := journal.NewFiles(filepath.Join(config.State, "journal"))
	if err != nil {
		return TransactionHandle{}, err
	}
	j, err := journals.For(ctx, record.Journal)
	if err != nil {
		return TransactionHandle{}, err
	}
	h := &held{log: log, journals: journals, journal: j, lease: l}
	lifetime := config.TimeoutSeconds
	if lifetime == 0 {
		lifetime = 900
	}
	lifetimeCtx, stopLifetime := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(lifetime)*time.Second)
	h.maintain(lifetimeCtx, time.Duration(config.LeaseSeconds)*time.Second)
	release := h.release
	h.release = func() { release(); stopLifetime() }
	defer func() {
		if !keep {
			h.release()
		}
	}()
	if pendingStatus {
		if _, err = s.authorize(ctx, config.Collection, "transaction.recover"); err != nil {
			return TransactionHandle{}, err
		}
		if err = log.Resolve(ctx, key, []byte(id), nil, current); err != nil {
			return TransactionHandle{}, err
		}
	}
	if record.Status == "committed" || record.Status == "rolled_back" {
		return TransactionHandle{}, fmt.Errorf("transaction is closed: %s", record.Status)
	}
	_, current, _, err = log.Get(ctx, key)
	if err != nil {
		return TransactionHandle{}, err
	}
	record.Attachment = h.lease.Holder()
	t := &transaction{version: current, generation: rand.Text(), config: config, record: record, held: h}
	t.run = &managedRun{name: config.Collection, ledger: log, journal: j, held: h}
	for _, e := range record.Effects {
		t.run.applied = append(t.run.applied, appliedEffect{key: facade.LedgerKey(e.Key), address: e.Address, verb: e.Verb, identity: e.Identity, version: e.Version, inverseMethod: e.InverseMethod, inverseParams: maps.Clone(e.Scope), resource: NewManaged(strings.TrimPrefix(e.Key, config.Collection+"/"), nil, e.IdentityColumn, e.AddressedBy)})
	}
	if record.Status == "executing" || record.Running || len(record.InFlight) > 0 {
		t.recoveryOnly = true
		t.failed = fmt.Errorf("transaction requires verified statement outcome recovery")
	} else if record.Status == "aborted" {
		t.failed = fmt.Errorf("resumed aborted transaction; rollback required")
	}
	for _, effect := range t.run.applied {
		entry, version, found, readErr := log.Get(ctx, effect.key)
		if readErr != nil {
			return TransactionHandle{}, readErr
		}
		if !found || entry.Phase() != facade.LedgerLive || version != effect.version || string(entry.Identity()) != effect.identity {
			t.failed = fmt.Errorf("resource %s changed while transaction was detached; rollback requires reconciliation", effect.key)
			t.record.Status = "aborted"
			if err = saveTransaction(ctx, t, id); err != nil {
				return TransactionHandle{}, err
			}
			break
		}
	}
	if err = saveTransaction(ctx, t, id); err != nil {
		return TransactionHandle{}, err
	}
	s.active[id] = t
	keep = true
	return TransactionHandle{id: id, generation: t.generation, session: s}, nil
}

// Plan binds one host-translated graph to this session's capability. The host
// still translates SQL; execution and compensation belong to the transaction.
func (s *TransactionSession) Plan(handle TransactionHandle, registry string, g Graph, managed []Managed, args Args) Plan {
	return transactionPlan{session: s, handle: handle, registry: registry, g: g, managed: managed, args: args}
}

type transactionPlan struct {
	session  *TransactionSession
	handle   TransactionHandle
	registry string
	g        Graph
	managed  []Managed
	args     Args
}

func (p transactionPlan) Open(ctx context.Context) (Rows, error) {
	if len(p.managed) == 0 && transactionReadGraph(p.g) {
		return p.session.openRead(ctx, p.handle, p.registry, p.g, p.args)
	}
	return p.session.Execute(ctx, p.handle, p.registry, p.g, p.managed, p.args)
}

// Queue durably defers a host-translated statement payload. Payloads must contain
// no credentials. The host owns syntax translation, not execution scheduling.
func (s *TransactionSession) Queue(ctx context.Context, handle TransactionHandle, payload json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.lookup(handle)
	if err != nil {
		return err
	}
	ctx, err = s.authorize(ctx, t.config.Collection, "transaction.execute")
	if err != nil {
		return err
	}
	if t.failed != nil {
		return fmt.Errorf("transaction is aborted; rollback required")
	}
	if t.record.Running {
		return fmt.Errorf("transaction is executing another statement")
	}
	if t.config.Mode != TransactionLazy {
		return fmt.Errorf("eager transaction cannot defer statements")
	}
	if !json.Valid(payload) {
		return fmt.Errorf("transaction payload must be JSON")
	}
	if err = t.held.check(ctx); err != nil {
		return err
	}
	t.record.Queue = append(t.record.Queue, append(json.RawMessage(nil), payload...))
	if err = saveTransaction(ctx, t, handle.id); err != nil {
		t.failed = err
		return err
	}
	return nil
}

// Drain is the dependency barrier used before reads and COMMIT. A queued
// statement is marked running durably before translation/execution; a crash in
// that interval requires outcome recovery and never permits automatic replay.
func (s *TransactionSession) Drain(ctx context.Context, handle TransactionHandle, execute func(context.Context, json.RawMessage) error) error {
	if execute == nil {
		return fmt.Errorf("transaction translator is required")
	}
	for {
		s.mu.Lock()
		t, err := s.lookup(handle)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		runCtx, err := s.authorize(ctx, t.config.Collection, "transaction.execute")
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if t.failed != nil {
			s.mu.Unlock()
			return fmt.Errorf("transaction is aborted; rollback required: %w", t.failed)
		}
		if t.record.Running {
			s.mu.Unlock()
			return fmt.Errorf("transaction statement already running or requires recovery")
		}
		if len(t.record.Queue) == 0 {
			s.mu.Unlock()
			return nil
		}
		t.record.Running = true
		payload := append(json.RawMessage(nil), t.record.Queue[0]...)
		err = saveTransaction(runCtx, t, handle.id)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		err = execute(context.WithValue(runCtx, transactionWorkerKey{}, t), payload)
		s.mu.Lock()
		if _, lookupErr := s.lookup(handle); lookupErr != nil {
			s.mu.Unlock()
			return lookupErr
		}
		if err != nil {
			t.failed = err
			t.record.Status = "aborted"
		} else {
			t.record.Queue = t.record.Queue[1:]
		}
		t.record.Running = false
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(runCtx), 5*time.Second)
		persisted := saveTransaction(cleanup, t, handle.id)
		cancel()
		if persisted != nil {
			t.failed = errors.Join(err, persisted)
		}
		s.mu.Unlock()
		if err != nil || persisted != nil {
			return errors.Join(err, persisted)
		}
	}
}

// Configuration returns the durable addressing declaration after authorization.
func (s *TransactionSession) Configuration(ctx context.Context, handle TransactionHandle) (TransactionConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.lookup(handle)
	if err != nil {
		return TransactionConfig{}, err
	}
	if _, err = s.authorize(ctx, t.config.Collection, "transaction.inspect"); err != nil {
		return TransactionConfig{}, err
	}
	if err = t.held.check(ctx); err != nil {
		return TransactionConfig{}, err
	}
	raw, err := json.Marshal(t.config)
	if err != nil {
		return TransactionConfig{}, err
	}
	var out TransactionConfig
	err = json.Unmarshal(raw, &out)
	return out, err
}

func inverseDocument(registry, address string, args Args) (string, error) {
	table, err := DescribeMutation(registry, address, "delete", args.documentOptions()...)
	if err != nil {
		return "", err
	}
	var identities []string
	for _, method := range table.Methods() {
		signature, ok := method.(docSignature)
		if !ok {
			return "", fmt.Errorf("inverse does not expose document identity")
		}
		declared, ok := signature.ex.(aot.DocumentIdentity)
		if !ok || declared.DocumentIdentity() == "" {
			return "", fmt.Errorf("inverse does not expose document identity")
		}
		identities = append(identities, method.Method()+":"+declared.DocumentIdentity())
	}
	if len(identities) == 0 {
		return "", fmt.Errorf("transaction create has no inverse")
	}
	sort.Strings(identities)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(identities, "\n")))), nil
}

func (s *TransactionSession) openRead(ctx context.Context, handle TransactionHandle, registry string, g Graph, args Args) (Rows, error) {
	s.mu.Lock()
	t, err := s.lookup(handle)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	ctx, err = s.authorize(ctx, t.config.Collection, "transaction.execute")
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if t.failed != nil || len(t.record.Queue) > 0 || t.record.Running {
		s.mu.Unlock()
		return nil, fmt.Errorf("transaction must drain pending work before reading")
	}
	for _, n := range g.Nodes() {
		if n.Verb() != "select" {
			s.mu.Unlock()
			return nil, fmt.Errorf("transaction mutation has no managed target")
		}
	}
	if err = t.held.check(ctx); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	args.Caller = nil
	args.RequireCaller = facade.AccessEnabled(ctx) || args.RequireCaller
	args.ReadCache = nil
	args.TxnRollback = false
	args.IACRollback = false
	args.Journal = nil
	plan, err := NewGraphQuery(registry, g, args)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	t.readers++
	s.mu.Unlock()
	release := func() { s.mu.Lock(); defer s.mu.Unlock(); t.readers-- }
	readCtx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(t.held.ctx, func() { cancel(context.Cause(t.held.ctx)) })
	rows, err := plan.Open(readCtx)
	if err != nil {
		stop()
		cancel(nil)
		release()
		return nil, err
	}
	return &transactionRows{Rows: rows, release: func() { stop(); cancel(nil); release() }}, nil
}

type transactionRows struct {
	closeErr error
	Rows
	once    sync.Once
	release func()
}

func (r *transactionRows) Next() bool {
	next := r.Rows.Next()
	if !next {
		_ = r.Close()
	}
	return next
}
func (r *transactionRows) Close() error {
	r.once.Do(func() { r.closeErr = r.Rows.Close(); r.release() })
	return r.closeErr
}

// Abort marks a host translation/admission failure without claiming compensation.
func (s *TransactionSession) Abort(ctx context.Context, handle TransactionHandle, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.lookup(handle)
	if err != nil {
		return err
	}
	ctx, err = s.authorize(ctx, t.config.Collection, "transaction.execute")
	if err != nil {
		return err
	}
	if cause == nil {
		return fmt.Errorf("abort requires a cause")
	}
	t.failed = cause
	t.record.Status = "aborted"
	return saveTransaction(ctx, t, handle.id)
}

func captureInverseScope(destination, source map[string]string) {
	for key, value := range source {
		if !docx.IsCredential(key) && !strings.HasPrefix(key, "\x00") {
			if _, present := destination[key]; !present {
				destination[key] = value
			}
		}
	}
}

// TransactionPending identifies an uncertain resource version for explicit
// provider verification. A version/identity is evidence, not replay permission.
type TransactionPending struct {
	Key      string `json:"key"`
	Version  string `json:"version"`
	Identity string `json:"identity,omitempty"`
}
type TransactionSnapshot struct {
	ID      string               `json:"id"`
	Status  string               `json:"status"`
	Version string               `json:"version"`
	Pending []TransactionPending `json:"pending,omitempty"`
}

func (s *TransactionSession) Status(ctx context.Context, handle TransactionHandle) (TransactionSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.lookup(handle)
	if err != nil {
		return TransactionSnapshot{}, err
	}
	ctx, err = s.authorize(ctx, t.config.Collection, "transaction.inspect")
	if err != nil {
		return TransactionSnapshot{}, err
	}
	if err = t.held.check(ctx); err != nil {
		return TransactionSnapshot{}, err
	}
	_, version, _, err := t.held.log.Get(ctx, transactionKey(t.config.Collection, handle.id))
	if err != nil {
		return TransactionSnapshot{}, err
	}
	out := TransactionSnapshot{ID: handle.id, Status: t.record.Status, Version: string(version)}
	for _, intent := range t.record.InFlight {
		entry, version, found, err := t.held.log.Get(ctx, t.run.keyOf(intent.Key))
		if err != nil {
			return TransactionSnapshot{}, err
		}
		pending := TransactionPending{Key: intent.Key, Version: string(version)}
		if found {
			pending.Identity = string(entry.Identity())
		}
		out.Pending = append(out.Pending, pending)
	}
	return out, nil
}

type TransactionResolution struct {
	Key           string            `json:"key"`
	Outcome       string            `json:"outcome"`
	Version       string            `json:"version"`
	Identity      string            `json:"identity,omitempty"`
	InverseParams map[string]string `json:"inverse_params,omitempty"`
}
type TransactionRecovery struct {
	Version   string                  `json:"version"`
	Resources []TransactionResolution `json:"resources"`
	// Continue explicitly attests that the interrupted statement completed.
	// False aborts it; subsequent rollback compensates verified applied effects.
	Continue bool `json:"continue"`
}

// Recover applies an authorized operator's verified provider outcomes. It makes
// no provider request and records each assertion before its resource CAS. Every
// uncertain resource must be covered; no request is replayed by this method.
func (s *TransactionSession) Recover(ctx context.Context, handle TransactionHandle, decision TransactionRecovery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.lookup(handle)
	if err != nil {
		return err
	}
	ctx, err = s.authorize(ctx, t.config.Collection, "transaction.recover")
	if err != nil {
		return err
	}
	if !t.recoveryOnly {
		return fmt.Errorf("resume the interrupted transaction before recovery")
	}
	if err = t.held.check(ctx); err != nil {
		return err
	}
	_, current, _, err := t.held.log.Get(ctx, transactionKey(t.config.Collection, handle.id))
	if err != nil {
		return err
	}
	if current != t.version || string(current) != decision.Version {
		return facade.ErrLedgerConflict
	}
	answers := map[string]TransactionResolution{}
	for _, answer := range decision.Resources {
		if _, duplicate := answers[answer.Key]; duplicate {
			return fmt.Errorf("duplicate recovery resource %s", answer.Key)
		}
		answers[answer.Key] = answer
	}
	if len(answers) != len(t.record.InFlight) {
		return fmt.Errorf("recovery must cover every uncertain resource")
	}
	type resolution struct {
		intent  transactionIntent
		answer  TransactionResolution
		entry   facade.LedgerEntry
		version facade.LedgerVersion
		found   bool
		params  map[string]string
		about   []byte
	}
	var resolutions []resolution
	for _, intent := range t.record.InFlight {
		answer, found := answers[intent.Key]
		if !found {
			return fmt.Errorf("missing recovery outcome for %s", intent.Key)
		}
		if answer.Outcome != "applied" && answer.Outcome != "absent" {
			return fmt.Errorf("recovery outcome must be applied or absent")
		}
		if decision.Continue && answer.Outcome != "applied" {
			return fmt.Errorf("continuation requires verified applied outcomes")
		}
		entry, version, found, err := t.held.log.Get(ctx, t.run.keyOf(intent.Key))
		if err != nil {
			return err
		}
		if string(version) != answer.Version {
			return facade.ErrLedgerConflict
		}
		item := resolution{intent: intent, answer: answer, entry: entry, version: version, found: found}
		if answer.Outcome == "applied" {
			if !found || answer.Identity == "" {
				return fmt.Errorf("applied recovery requires durable intent and verified identity")
			}
			if entry.Phase() == facade.LedgerLive && string(entry.Identity()) != answer.Identity {
				return fmt.Errorf("verified identity differs from the resolved resource")
			}
			item.params = maps.Clone(intent.Scope)
			if item.params == nil {
				item.params = map[string]string{}
			}
			for name, token := range intent.Tokens {
				value := answer.InverseParams[name]
				if value == "" && token.Key == intent.IdentityColumn {
					value = answer.Identity
				}
				if value == "" {
					return fmt.Errorf("verified inverse token %s is required", name)
				}
				item.params[name] = value
			}
			item.about = entry.About()
			if len(item.about) == 0 {
				item.about, err = journal.RecoveryAbout(ctx, filepath.Join(t.config.State, "journal"), t.run.keyOf(intent.Key))
				if err != nil {
					return err
				}
			}
			if len(item.about) == 0 {
				return fmt.Errorf("resource recovery addressing is missing")
			}
			var address aboutRecord
			if json.Unmarshal(item.about, &address) == nil {
				captureInverseScope(item.params, address.Params)
			}
			var actual map[string]any
			request := entry.Plan()
			if entry.Phase() == facade.LedgerPending {
				request = entry.Proposed()
			}
			if json.Unmarshal(request, &actual) == nil {
				for name, value := range actual {
					if value != nil && !docx.IsCredential(name) && !strings.HasPrefix(name, "\x00") {
						if _, exists := item.params[name]; !exists {
							item.params[name] = fmt.Sprint(value)
						}
					}
				}
			}
		}
		resolutions = append(resolutions, item)
	}
	for _, item := range resolutions {
		raw, _ := json.Marshal(item.answer)
		if _, err = t.held.journal.Append(ctx, t.run.keyOf(item.intent.Key), "transaction.recovery", facade.FormRead, raw); err != nil {
			return err
		}
		if err = t.held.check(ctx); err != nil {
			return err
		}
		if item.answer.Outcome == "absent" {
			if item.found {
				err = t.held.log.Forget(ctx, t.run.keyOf(item.intent.Key), item.version)
			}
		} else {
			if item.entry.Phase() == facade.LedgerPending {
				err = t.held.log.Resolve(ctx, t.run.keyOf(item.intent.Key), []byte(item.answer.Identity), item.about, item.version)
			}
			if err == nil {
				_, version, _, readErr := t.held.log.Get(ctx, t.run.keyOf(item.intent.Key))
				if readErr != nil {
					return readErr
				}
				known := false
				for _, effect := range t.run.applied {
					if effect.key == t.run.keyOf(item.intent.Key) && effect.version == version {
						known = true
					}
				}
				if !known {
					t.run.applied = append(t.run.applied, appliedEffect{key: t.run.keyOf(item.intent.Key), address: item.intent.Address, verb: "insert", identity: item.answer.Identity, version: version,
						resource: NewManaged(item.intent.Key, nil, item.intent.IdentityColumn, item.intent.AddressedBy), inverseMethod: item.intent.InverseMethod, inverseParams: item.params})
				}
			}
		}
		if err != nil {
			return err
		}
	}
	t.record.InFlight = nil
	if decision.Continue {
		if t.record.Running && len(t.record.Queue) > 0 {
			t.record.Queue = t.record.Queue[1:]
		}
		t.record.Status = "active"
		t.failed = nil
	} else {
		t.record.Status = "aborted"
		t.failed = fmt.Errorf("verified interrupted transaction aborted; rollback required")
		t.record.Queue = nil
	}
	t.record.Running = false
	if err = saveTransaction(ctx, t, handle.id); err != nil {
		t.failed = err
		return err
	}
	t.recoveryOnly = false
	return nil
}

// BeginWithRegistry freezes the declared target documents and checks inverse
// authority before acquiring a transaction footprint. Stackql uses this entry
// point; callers constructing managed graphs directly may validate at Execute.
func (s *TransactionSession) BeginWithRegistry(ctx context.Context, config TransactionConfig, registry string, args Args) (TransactionHandle, error) {
	ctx, err := s.authorize(ctx, config.Collection, "transaction.begin")
	if err != nil {
		return TransactionHandle{}, err
	}
	documents := map[string]string{}
	config.Keys = append([]string(nil), config.Keys...)
	for address, target := range config.Targets {
		if _, _, binding, err := args.binding(address); err != nil {
			return TransactionHandle{}, err
		} else if binding != nil {
			return TransactionHandle{}, fmt.Errorf("transactional provider aliases require durable tenancy bindings")
		}
		declaration, err := DocInverse(registry, address, "insert", args.documentOptions()...)
		if err != nil {
			return TransactionHandle{}, err
		}
		if declaration == nil {
			return TransactionHandle{}, fmt.Errorf("transaction mutation %s has no declared inverse", address)
		}
		for name, token := range declaration.Tokens() {
			if token.Location != "body" || token.Algorithm != "toString" || token.Key == "" {
				return TransactionHandle{}, fmt.Errorf("unsupported inverse token %s", name)
			}
		}
		if _, err = s.authorize(ctx, config.Collection, "transaction.rollback"); err != nil {
			return TransactionHandle{}, err
		}
		ctx, err = authorizeInverse(ctx, registry, address, declaration, args)
		if err != nil {
			return TransactionHandle{}, err
		}
		fingerprint, err := inverseDocument(registry, address, args)
		if err != nil {
			return TransactionHandle{}, err
		}
		documents[address] = fingerprint
		if !config.DynamicScope {
			found := false
			for _, key := range config.Keys {
				if key == target.Key {
					found = true
				}
			}
			if !found {
				config.Keys = append(config.Keys, target.Key)
			}
		}
	}
	handle, err := s.Begin(ctx, config)
	if err != nil {
		return TransactionHandle{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.lookup(handle)
	if err != nil {
		return TransactionHandle{}, err
	}
	t.record.Documents = documents
	t.record.Endpoint = args.Endpoint
	t.record.EndpointBound = true
	if err = saveTransaction(ctx, t, handle.id); err != nil {
		t.held.release()
		delete(s.active, handle.id)
		return TransactionHandle{}, err
	}
	return handle, nil
}
func authorizeInverse(ctx context.Context, registry, address string, declaration DeclaredInverse, args Args) (context.Context, error) {
	described, err := DescribeMutation(registry, address, "delete", args.documentOptions()...)
	if err != nil {
		return nil, err
	}
	compiled, ok := described.(table)
	if !ok {
		return nil, fmt.Errorf("inverse permission metadata is unavailable")
	}
	for _, signature := range compiled.Methods() {
		if signature.Method() != declaration.Method() {
			continue
		}
		method, ok := signature.(docSignature)
		if !ok {
			return nil, fmt.Errorf("inverse permission metadata is unavailable")
		}
		return facade.Authorize(ctx, facade.DocumentPermissions(compiled.catalog.Provider().Name(), method.ex.Name(), method.ex.OperationID())...)
	}
	return nil, fmt.Errorf("declared inverse not found")
}

// transactionReadGraph keeps only read graphs on the streaming read path.
func transactionReadGraph(g Graph) bool {
	for _, node := range g.Nodes() {
		if node.Verb() != "select" {
			return false
		}
	}
	return true
}

// Implicit creates get distinct durable intent keys, plus a conservative shared
// resource-address lock. Nothing is inferred about an undeclared inverse.
func implicitTransactionManaged(registry string, g Graph, id string, offset int, args Args) ([]Managed, []string, error) {
	var managed []Managed
	var locks []string
	for i, node := range g.Nodes() {
		if node.Verb() == "select" {
			continue
		}
		if node.Verb() != "insert" {
			return nil, nil, fmt.Errorf("transaction has no verified inverse for %s", node.Verb())
		}
		inverse, err := DocInverse(registry, node.Address(), "insert", args.documentOptions()...)
		if err != nil {
			return nil, nil, err
		}
		if inverse == nil {
			return nil, nil, fmt.Errorf("transaction create requires a declared inverse")
		}
		tokens := inverse.Tokens()
		if len(tokens) != 1 {
			return nil, nil, fmt.Errorf("transaction target %s requires explicit identity addressing", node.Address())
		}
		for parameter, token := range tokens {
			if token.Location != "body" || token.Algorithm != "toString" || token.Key == "" {
				return nil, nil, fmt.Errorf("transaction target %s requires explicit identity addressing", node.Address())
			}
			key := fmt.Sprintf("transactions/%s/%d/%s", id, offset+i, node.Alias())
			managed = append(managed, NewManaged(key, []string{node.Alias()}, token.Key, parameter))
		}
		locks = append(locks, "resources/"+node.Address())
	}
	return managed, locks, nil
}
