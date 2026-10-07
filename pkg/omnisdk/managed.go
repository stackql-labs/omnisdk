package omnisdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/internal/apply"
	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/merge"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/retry"
)

// A converging graph is IaC written as a query: recall what the ledger knows of a resource, read it
// live, diff the two against what is wanted, branch on the difference, and run whichever mutation the
// arm calls for. Any mutation a document declares can sit on an arm — create, update, delete, or a
// sequence of them — and each runs with the guarantees a converging run gives: the collection is
// held, intent is in the ledger before every request, and a failed run compensates what it did.

const (
	verbRecall = "recall"
	verbDiff   = "diff"
)

// Diff statuses: what a diff found, for a branch to choose on.
const (
	// DiffAbsent: the live read found nothing.
	DiffAbsent = "absent"
	// DiffMatch: the live object already satisfies what is wanted; nothing to send.
	DiffMatch = "match"
	// DiffDrift: the live object exists and differs; the diff's fields are what to send.
	DiffDrift = "drift"
)

type intrinsicNode struct {
	node
	key          string
	live         string
	liveIdentity string
}

// NewRecallNode is what the ledger knows of resource key in the run's collection: column identity
// (empty where the ledger holds none) for a read to address the live object by. It makes no request,
// and only runs in a converging graph.
func NewRecallNode(alias, key string) Node {
	return intrinsicNode{node: node{alias: alias, verb: verbRecall}, key: key}
}

// NewDiffNode compares desired with the live object node live read, against the intent the ledger
// last recorded for key — so a field dropped from desired is unset, and a field nobody asked for is
// left alone. liveIdentity is the column of live that is empty when nothing was found; live must be
// an outer node, so an absent object still reaches the diff. Its columns are status — DiffAbsent,
// DiffMatch or DiffDrift — identity, and each field to send.
func NewDiffNode(alias, key, live, liveIdentity string, desired map[string]any) Node {
	return intrinsicNode{node: node{alias: alias, verb: verbDiff, body: maps.Clone(desired)}, key: key,
		live: live, liveIdentity: liveIdentity}
}

// Managed names the mutation nodes that act on one resource of the collection, and what the ledger
// needs of their responses. Every mutation node of a converging graph belongs to one.
type Managed interface {
	// Key is the resource's name within the collection.
	Key() string
	// Aliases are its mutation nodes.
	Aliases() []string
	// Identity is the column of a create's row holding what it minted.
	Identity() string
	// AddressedBy is the parameter that addresses an existing object, so a create can be reversed.
	AddressedBy() string
}

// NewManaged declares the mutation nodes acting on resource key.
func NewManaged(key string, aliases []string, identity, addressedBy string) Managed {
	return managed{key: key, aliases: slices.Clone(aliases), identity: identity, addressedBy: addressedBy}
}

type managed struct {
	key         string
	aliases     []string
	identity    string
	addressedBy string
}

func (m managed) Key() string         { return m.key }
func (m managed) Aliases() []string   { return m.aliases }
func (m managed) Identity() string    { return m.identity }
func (m managed) AddressedBy() string { return m.addressedBy }

// managedRun is the durable state one converging run shares with the graph it runs.
type managedRun struct {
	name    string
	ledger  facade.Ledger
	journal facade.Journal
	nodes   map[string]Managed // alias → resource

	mu      sync.Mutex
	applied []appliedEffect // in the order they landed
}

type appliedEffect struct {
	key      facade.LedgerKey
	resource Managed
	address  string
	verb     string
	identity string
}

func (r *managedRun) keyOf(k string) facade.LedgerKey { return facade.LedgerKey(r.name + "/" + k) }

// intrinsicSpec compiles a recall or diff node.
func intrinsicSpec(name string, n intrinsicNode, run *managedRun, planned map[string]string) (plan.ExchangeSpec, []plan.BetaEdge, error) {
	if run == nil {
		return nil, nil, fmt.Errorf("omnisdk: %s: a %s node runs only in a converging graph (ConvergeGraph)", n.Alias(), n.Verb())
	}
	out := func(attr string) string { return hidden(n.Alias(), attr) }
	switch n.Verb() {
	case verbRecall:
		return plan.NewExchangeSpec(name, nil, []string{out("identity")}, func(map[string]any) facade.Operator {
			row := map[string]any{out("identity"): ""}
			if run.ledger != nil {
				e, _, found, err := run.ledger.Get(context.Background(), run.keyOf(n.key))
				if err != nil {
					return failedOp{err}
				}
				if found {
					row[out("identity")] = string(e.Identity())
				}
			}
			return staticOp{rows: []map[string]any{row}}
		}, nil), nil, nil
	case verbDiff:
		from, ok := planned[n.live]
		if !ok {
			return nil, nil, fmt.Errorf("omnisdk: diff %q reads %q, which the graph does not include", n.Alias(), n.live)
		}
		fields := slices.Sorted(maps.Keys(n.Body()))
		var in []string
		var betas []plan.BetaEdge
		for _, f := range append(slices.Clone(fields), n.liveIdentity) {
			key := hidden(n.live, f)
			if !slices.Contains(in, key) {
				in = append(in, key)
				betas = append(betas, plan.NewBetaEdge(from, name, key, key))
			}
		}
		desired, err := json.Marshal(n.Body())
		if err != nil {
			return nil, nil, fmt.Errorf("omnisdk: diff %q: %w", n.Alias(), err)
		}
		return plan.NewExchangeSpec(name, in, nil, func(bound map[string]any) facade.Operator {
			row, err := diffRow(run, n, fields, desired, bound)
			if err != nil {
				return failedOp{err}
			}
			return staticOp{rows: []map[string]any{row}}
		}, nil), betas, nil
	}
	return nil, nil, fmt.Errorf("omnisdk: %s: unknown intrinsic %q", n.Alias(), n.Verb())
}

// diffRow is one diff: the status and what to send, from the live values bound and the ledger's prior.
func diffRow(run *managedRun, n intrinsicNode, fields []string, desired []byte, bound map[string]any) (map[string]any, error) {
	out := func(attr string) string { return hidden(n.Alias(), attr) }
	id := fmt.Sprint(bound[hidden(n.live, n.liveIdentity)])
	if bound[hidden(n.live, n.liveIdentity)] == nil {
		id = ""
	}
	actual := map[string]any{}
	for _, f := range fields {
		if v, ok := bound[hidden(n.live, f)]; ok && v != nil {
			actual[f] = v
		}
	}
	var prior []byte
	if run.ledger != nil {
		e, _, found, err := run.ledger.Get(context.Background(), run.keyOf(n.key))
		if err != nil {
			return nil, err
		}
		if found {
			prior = e.Plan()
		}
	}
	live, err := json.Marshal(actual)
	if err != nil {
		return nil, err
	}
	row := map[string]any{out("identity"): id}
	if id == "" {
		row[out("status")] = DiffAbsent
		var want map[string]any
		_ = json.Unmarshal(desired, &want)
		for k, v := range want {
			row[out(k)] = v
		}
		return row, nil
	}
	mutation, err := merge.ThreeWay().Apply(prior, desired, live)
	if err != nil {
		return nil, fmt.Errorf("omnisdk: diff %q: %w", n.Alias(), err)
	}
	same, err := apply.Satisfied(mutation, live)
	if err != nil {
		return nil, fmt.Errorf("omnisdk: diff %q: %w", n.Alias(), err)
	}
	row[out("status")] = DiffDrift
	if same {
		row[out("status")] = DiffMatch
	}
	var send map[string]any
	if err := json.Unmarshal(mutation, &send); err != nil {
		return nil, fmt.Errorf("omnisdk: diff %q: %w", n.Alias(), err)
	}
	for k, v := range send {
		row[out(k)] = v
	}
	return row, nil
}

// durable wraps a managed mutation's request in the ledger: intent before it, identity after it, so a
// run that dies between the two leaves something a later run knows to ask about.
type durable struct {
	run      *managedRun
	resource Managed
	address  string
}

func (d durable) open(ctx context.Context, o effectOp) facade.Records {
	k := d.run.keyOf(d.resource.Key())
	e, v, found, err := d.run.ledger.Get(ctx, k)
	if err != nil {
		return failed(err)
	}
	if !found {
		v = facade.LedgerVersionNone
	}
	request := make(map[string]any, len(o.bound))
	for name, val := range o.bound {
		if !strings.HasPrefix(name, "\x00") {
			request[name] = val
		}
	}
	proposed, err := json.Marshal(request)
	if err != nil {
		return failed(err)
	}
	notAttempted := func(err error) facade.Records {
		return failed(&EffectError{Outcome: ErrNotAttempted, Verb: o.e.verb, Alias: o.e.alias,
			Cause: fmt.Errorf("recording its intent failed: %w", err)})
	}
	var prior []byte
	if found {
		prior = e.Plan()
	}
	if o.e.verb != "delete" {
		if err := d.run.ledger.Begin(ctx, k, proposed, v); err != nil {
			return notAttempted(err)
		}
	}
	if _, err := d.run.journal.Append(ctx, k, o.e.exchange, formOf(o.e.verb), prior); err != nil {
		return notAttempted(err)
	}
	return &durableRecords{inner: effectRecords{Records: o.inner.Open(retryNever(ctx)), op: o}, d: d, key: k, entry: e, found: found}
}

type durableRecords struct {
	inner effectRecords
	d     durable
	key   facade.LedgerKey
	entry facade.LedgerEntry
	found bool
	first map[string]any
	done  bool
	err   error
}

func (r *durableRecords) Next(ctx context.Context) bool {
	if r.done {
		return false
	}
	if r.inner.Next(ctx) {
		if r.first == nil {
			r.first, _ = bind.DocMap(r.inner.Record())
		}
		return true
	}
	r.done = true
	if err := r.inner.Err(); err != nil {
		r.err = err
		return false
	}
	r.err = r.commit(ctx)
	return false
}

// commit records what landed: a create's identity, an update's new intent, a delete's absence.
func (r *durableRecords) commit(ctx context.Context) error {
	identity := ""
	if r.found {
		identity = string(r.entry.Identity())
	}
	if v, ok := r.first[r.d.resource.Identity()]; ok && v != nil && fmt.Sprint(v) != "" {
		identity = fmt.Sprint(v)
	}
	_, ver, found, err := r.d.run.ledger.Get(ctx, r.key)
	if err != nil {
		return err
	}
	op := r.inner.op
	switch op.e.verb {
	case "delete":
		if found {
			if err := r.d.run.ledger.Forget(ctx, r.key, ver); err != nil {
				return err
			}
		}
	default:
		if !found {
			return fmt.Errorf("omnisdk: %s vanished between intent and commit", r.key)
		}
		if err := r.d.run.ledger.Resolve(ctx, r.key, []byte(identity), ver); err != nil {
			return err
		}
	}
	r.d.run.mu.Lock()
	r.d.run.applied = append(r.d.run.applied, appliedEffect{key: r.key, resource: r.d.resource, address: r.d.address,
		verb: op.e.verb, identity: identity})
	r.d.run.mu.Unlock()
	return nil
}

func (r *durableRecords) Record() facade.Record { return r.inner.Record() }
func (r *durableRecords) Err() error            { return r.err }
func (r *durableRecords) Close() error          { return r.inner.Close() }

// ConvergeGraph runs g as an IaC run over collection name, its ledger and journals under state. Every
// mutation node must belong to one of managed. The collection is held for the run; intent is recorded
// before every request; and a run that fails compensates what it did — a create is deleted by its
// resource's AddressedBy — reporting what it could not reverse. Opening the plan performs the run;
// its rows are the graph's rows, and a failed run adds a final row saying what was compensated.
func ConvergeGraph(registry, name, state, runID string, g Graph, managed []Managed, args Args) (Plan, error) {
	switch {
	case name == "":
		return nil, fmt.Errorf("omnisdk: collection name is required")
	case state == "":
		return nil, fmt.Errorf("omnisdk: state directory is required")
	}
	nodes := map[string]Managed{}
	for _, m := range managed {
		if m.Key() == "" {
			return nil, fmt.Errorf("omnisdk: a managed resource has no key")
		}
		for _, a := range m.Aliases() {
			if other, twice := nodes[a]; twice {
				return nil, fmt.Errorf("omnisdk: %q belongs to both %q and %q", a, other.Key(), m.Key())
			}
			nodes[a] = m
		}
	}
	for _, n := range g.Nodes() {
		_, isManaged := nodes[n.Alias()]
		mutating := n.Verb() != "select" && n.Verb() != verbRecall && n.Verb() != verbDiff
		switch {
		case mutating && !isManaged:
			return nil, fmt.Errorf("omnisdk: %s %s belongs to no managed resource; a converging run records every mutation", n.Verb(), n.Alias())
		case isManaged && !mutating:
			return nil, fmt.Errorf("omnisdk: %q is managed but does not mutate", n.Alias())
		}
	}
	for a := range nodes {
		if !slices.ContainsFunc(g.Nodes(), func(n Node) bool { return n.Alias() == a }) {
			return nil, fmt.Errorf("omnisdk: managed node %q is not in the graph", a)
		}
	}
	// Planned now, against no state, so a graph that cannot run is refused before anything is held.
	probe := args
	probe.managed = &managedRun{name: name, nodes: nodes}
	if _, err := NewGraphSelectQuery(registry, g, probe); err != nil {
		return nil, err
	}
	if runID == "" {
		runID = time.Now().UTC().Format("20060102T150405Z")
	}
	return &convergeGraphPlan{registry: registry, name: name, state: state, runID: runID, g: g, nodes: nodes, args: args}, nil
}

type convergeGraphPlan struct {
	registry, name, state, runID string
	g                            Graph
	nodes                        map[string]Managed
	args                         Args
}

func (p *convergeGraphPlan) Open(ctx context.Context) (Rows, error) {
	log, err := ledger.NewFile(filepath.Join(p.state, "ledger"))
	if err != nil {
		return nil, err
	}
	journals, err := journal.NewFiles(filepath.Join(p.state, "journal"))
	if err != nil {
		return nil, err
	}
	held, err := lease.NewLeaser(log, time.Now).Acquire(ctx, p.name, lease.All(), p.runID, leaseTTL)
	if errors.Is(err, facade.ErrLeaseHeld) {
		return nil, fmt.Errorf("omnisdk: collection %q is busy with another run", p.name)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = held.Release(ctx) }()
	j, err := journals.For(ctx, p.runID)
	if err != nil {
		return nil, err
	}
	run := &managedRun{name: p.name, ledger: log, journal: j, nodes: p.nodes}
	a := p.args
	a.managed = run
	pl, err := NewGraphSelectQuery(p.registry, p.g, a)
	if err != nil {
		return nil, err
	}
	rows, err := pl.Open(ctx)
	if err != nil {
		return nil, err
	}
	var out []Row
	for rows.Next() {
		out = append(out, rows.Row())
	}
	runErr := rows.Err()
	rows.Close()
	if runErr == nil {
		return &sliceRows{rows: out, i: -1}, nil
	}
	compensated, completed := p.compensate(ctx, run)
	out = append(out, Row{"status": "failed", "error": runErr.Error(),
		"compensated": keyStrings(compensated), "completed": keyStrings(completed)})
	return &sliceRows{rows: out, i: -1}, nil
}

// compensate reverses what the run did, latest first: a create is deleted through its resource's
// document, addressed by the identity it minted. A provider refusing a delete — a dependent still
// there — is retried on the next pass, until a pass makes no progress. An update or delete is not
// reversed: it completed, and is reported so.
func (p *convergeGraphPlan) compensate(ctx context.Context, run *managedRun) (done, left []facade.LedgerKey) {
	run.mu.Lock()
	pending := slices.Clone(run.applied)
	run.mu.Unlock()
	slices.Reverse(pending)
	var todo []appliedEffect
	for _, e := range pending {
		if e.verb == "insert" && e.identity != "" && e.resource.AddressedBy() != "" {
			todo = append(todo, e)
		} else {
			left = append(left, e.key)
		}
	}
	undo := p.args
	undo.managed = nil
	for progress := true; progress && len(todo) > 0; {
		progress = false
		var next []appliedEffect
		for _, e := range todo {
			if err := deleteOne(ctx, p.registry, e, undo); err != nil {
				next = append(next, e)
				continue
			}
			if _, v, found, err := run.ledger.Get(ctx, e.key); err == nil && found {
				_ = run.ledger.Forget(ctx, e.key, v)
			}
			done = append(done, e.key)
			progress = true
		}
		todo = next
	}
	for _, e := range todo {
		left = append(left, e.key)
	}
	return done, left
}

func deleteOne(ctx context.Context, registry string, e appliedEffect, args Args) error {
	g, err := NewGraph([]Node{NewMutationNode("undo", e.address, "delete",
		map[string]string{e.resource.AddressedBy(): e.identity}, nil, nil)}, nil)
	if err != nil {
		return err
	}
	pl, err := NewGraphSelectQuery(registry, g, args)
	if err != nil {
		return err
	}
	rows, err := pl.Open(ctx)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// retryNever is ctx with retries off: a mutation's request repeated after it reached the provider
// repeats the effect.
func retryNever(ctx context.Context) context.Context { return retry.WithPolicy(ctx, never{}) }

// failedOp is an operator whose run fails with err.
type failedOp struct{ err error }

func (f failedOp) Open(context.Context) facade.Records { return failed(f.err) }
