package omnisdk

import (
	"context"
	"crypto/rand"
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
	"github.com/stackql-labs/omnisdk/internal/semantics"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/retry"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

// A converging graph is IaC written as a query: recall what the ledger knows of a resource, read it
// live, diff the two against what is wanted, branch on the difference, and run whichever mutation the
// arm calls for. Any mutation a document declares can sit on an arm — create, update, delete, or a
// sequence of them — and each runs with the guarantees a converging run gives: the collection is
// held and intent is in the ledger before every request. A failed run preserves
// partial progress unless Args.IACRollback explicitly enables compensation.

const (
	verbRecall   = "recall"
	verbDiff     = "diff"
	verbFunction = "function"
	verbShape    = "shape"
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
	// Update-only intent must never be silently dropped when an object is absent.
	createUnsupported []string
	node
	key          string
	live         string
	liveIdentity string
	// call is a function node's table function, its arguments reading the rows before it.
	call query.Call
	// route, on a diff, chooses the update for a drifted object: the method that converges it, in
	// column update, and the fields to send it. Nil sends the whole mutation and names no method.
	route driftRoute
	// A shape node's inbox, the program reshaping it, and the names the program provides.
	inbox            []string
	viaType, viaBody string
	provides         []string
}

// driftRoute chooses the update for a drift: given the merged mutation and the live object, the
// method to send and what to send it, or an error where no method converges it.
type driftRoute func(ctx context.Context, mutation, live map[string]any) (method string, send map[string]any, err error)

// newFunctionNode is a table function in FROM: call, run once per row before it, joined as outer
// says — kept unmatched under a left join — and matched by on.
func newFunctionNode(alias string, call query.Call, outer bool, on []query.Predicate) Node {
	return intrinsicNode{node: node{alias: alias, verb: verbFunction, outer: outer, on: on}, call: call}
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
	// For a resource lowered from desired state: identityPath is a dotted path into the create's
	// whole response, where identity is a column of its row; intent is the desired document, which
	// the ledger records as the resource's intent in place of the request's inputs.
	identityPath string
	intent       []byte
	// about is what the ledger records the resource is, for acting on it from the ledger alone.
	about []byte
	// creates are the aliases whose effect makes the object, whatever their verb: a replace that
	// makes an object is undone by deleting it, not by restoring an intent it never had.
	creates map[string]bool
}

// identityAttr is the column a managed mutation adds to its row: the identity the ledger holds for
// its resource once the mutation lands, for whatever depends on the resource to read.
const identityAttr = "\x00identity"

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
	locks   *resourceLocks
	held    *held

	mu            sync.Mutex
	applied       []appliedEffect // in the order they landed
	access        map[facade.LedgerKey]localAccess
	accessChanged chan struct{}
}

type appliedEffect struct {
	key      facade.LedgerKey
	resource Managed
	address  string
	verb     string
	identity string
	version  facade.LedgerVersion
}

func (r *managedRun) keyOf(k string) facade.LedgerKey { return facade.LedgerKey(r.name + "/" + k) }

// intrinsicSpec compiles a recall or diff node.
func intrinsicSpec(name string, n intrinsicNode, run *managedRun, planned map[string]string, reg dsl.Registry) (plan.ExchangeSpec, []plan.BetaEdge, error) {
	if run == nil {
		return nil, nil, fmt.Errorf("omnisdk: %s: a %s node runs only in a converging graph (ConvergeGraph)", n.Alias(), n.Verb())
	}
	out := func(attr string) string { return hidden(n.Alias(), attr) }
	switch n.Verb() {
	case verbShape:
		// T_in on its own: the inbox reshaped by the program, emitted as the names it provides for
		// the nodes after it. It makes no request.
		var outs []string
		for _, p := range n.provides {
			outs = append(outs, out(p))
		}
		spec := plan.NewExchangeSpec(name, n.inbox, outs, func(bound map[string]any) facade.Operator {
			row := map[string]any{}
			for _, p := range n.provides {
				row[out(p)] = bound[p]
			}
			return staticOp{rows: []map[string]any{row}}
		}, nil)
		return plan.WithInbound(spec, docx.InboundProgram(reg, n.viaType, n.viaBody)), nil, nil
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
			return diffOperator{run: run, node: n, fields: fields, desired: desired, bound: maps.Clone(bound)}
		}, nil), betas, nil
	}
	return nil, nil, fmt.Errorf("omnisdk: %s: unknown intrinsic %q", n.Alias(), n.Verb())
}

// diffRow is one diff: the status and what to send, from the live values bound and the ledger's prior.
func diffRow(ctx context.Context, run *managedRun, n intrinsicNode, fields []string, desired []byte, bound map[string]any) (map[string]any, error) {
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
		e, _, found, err := run.ledger.Get(ctx, run.keyOf(n.key))
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
		if len(n.createUnsupported) != 0 {
			return nil, fmt.Errorf("%s is absent: fields %v require an existing object; create it first, then apply this update", n.key, n.createUnsupported)
		}
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
	// Numbers stay as written: a value past float64's exact range is sent as it was asked for.
	send, err := decodeObject(mutation)
	if err != nil {
		return nil, fmt.Errorf("omnisdk: diff %q: %w", n.Alias(), err)
	}
	if !same && n.route != nil {
		method, routed, err := n.route(ctx, send, actual)
		if err != nil {
			return nil, fmt.Errorf("omnisdk: %s has drifted and no update path converges it: %w", n.key, err)
		}
		row[out("update")] = method
		send = routed
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
	// What the ledger records the resource is, where desired state has not already said: its
	// provider, the inputs its delete declares, and the keys it reads from.
	provider  string
	deletes   map[string]bool
	dependsOn []string
}

// aboutOf is what the ledger records a hand-written graph's resource is, from what its mutation was
// bound with: enough for a destroy to delete it from the ledger alone, and after what depends on it.
func (d durable) aboutOf(bound map[string]any) []byte {
	rel := d.address
	if i := strings.Index(rel, d.provider+"."); i >= 0 {
		rel = rel[i+len(d.provider)+1:]
	}
	rec := aboutRecord{Provider: d.provider, Address: rel, AddressedBy: d.resource.AddressedBy()}
	for k, v := range bound {
		base, _, _ := strings.Cut(k, ".")
		if (d.deletes[k] || d.deletes[base]) && k != rec.AddressedBy && !strings.HasPrefix(k, "\x00") && !docx.IsCredential(k) && v != nil {
			if rec.Params == nil {
				rec.Params = map[string]string{}
			}
			rec.Params[k] = fmt.Sprint(v)
		}
	}
	for _, k := range d.dependsOn {
		rec.DependsOn = append(rec.DependsOn, d.run.name+"/"+k)
	}
	b, _ := json.Marshal(rec)
	return b
}

// deleteInputs are the inputs any delete operation on addr declares.
func deleteInputs(c aot.Catalog, addr string) map[string]bool {
	out := map[string]bool{}
	ops, _ := c.Operations(addr, "delete")
	for _, op := range ops {
		for _, in := range op.Inputs() {
			out[in] = true
		}
		for _, p := range op.Request().Parameters() {
			out[p.Name()] = true
		}
	}
	return out
}

// dependsOn are the resources m reads from: walking back from its mutations along the graph's
// wirings, any node of another managed resource, or a recall of another key.
func dependsOn(g Graph, owners map[string]Managed, m Managed) []string {
	from := map[string][]string{}
	for _, w := range g.Wirings() {
		for _, in := range w.Inbound() {
			from[w.To()] = append(from[w.To()], in.From())
		}
	}
	recalls := map[string]string{}
	for _, n := range g.Nodes() {
		if in, ok := n.(intrinsicNode); ok && n.Verb() == verbRecall {
			recalls[n.Alias()] = in.key
		}
	}
	var out []string
	seen := map[string]bool{}
	queue := slices.Clone(m.Aliases())
	for len(queue) > 0 {
		a := queue[0]
		queue = queue[1:]
		if seen[a] {
			continue
		}
		seen[a] = true
		if o, ok := owners[a]; ok && o.Key() != m.Key() {
			out = append(out, o.Key())
			continue
		}
		if k, ok := recalls[a]; ok && k != m.Key() {
			out = append(out, k)
			continue
		}
		queue = append(queue, from[a]...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (d durable) open(ctx context.Context, o effectOp) facade.Records {
	k := d.run.keyOf(d.resource.Key())
	dependencies := make([]facade.LedgerKey, 0, len(d.dependsOn))
	for _, dependency := range d.dependsOn {
		dependencies = append(dependencies, d.run.keyOf(dependency))
	}
	unlock, err := d.run.lockMutation(ctx, k, dependencies)
	if err != nil {
		return failed(err)
	}
	transferred := false
	defer func() {
		if !transferred {
			unlock()
		}
	}()
	if d.run.held != nil {
		if err := d.run.held.check(ctx); err != nil {
			return failed(err)
		}
	}
	for _, dependency := range d.dependsOn {
		entry, _, found, err := d.run.ledger.Get(ctx, d.run.keyOf(dependency))
		if err != nil {
			return failed(err)
		}
		if found && entry.Phase() == facade.LedgerPending {
			return failed(fmt.Errorf("omnisdk: dependency %s has unresolved intent; recover it before mutation", dependency))
		}
	}
	e, v, found, err := d.run.ledger.Get(ctx, k)
	if err != nil {
		return failed(err)
	}
	if found && e.Phase() == facade.LedgerPending {
		return failed(fmt.Errorf("omnisdk: %s has unresolved intent; recover its outcome before another mutation", k))
	}
	if found && o.e.verb == "insert" {
		return failed(fmt.Errorf("omnisdk: %s already exists in the ledger; use reconciliation instead of replaying create", k))
	}
	if !found {
		v = facade.LedgerVersionNone
	}
	// Intent is what was asked for. Hidden plumbing and credentials travel on the row to make the
	// request, and are not it: a bearer token in the ledger is a live credential on disk.
	request := make(map[string]any, len(o.bound))
	for name, val := range o.bound {
		if !strings.HasPrefix(name, "\x00") && !docx.IsCredential(name) {
			request[name] = val
		}
	}
	proposed, err := json.Marshal(request)
	if err != nil {
		return failed(err)
	}
	if m, ok := d.resource.(managed); ok && len(m.intent) > 0 {
		proposed = m.intent
	}
	notAttempted := func(err error) facade.Records {
		return failed(&EffectError{Outcome: ErrNotAttempted, Verb: o.e.verb, Alias: o.e.alias,
			Cause: fmt.Errorf("recording its intent failed: %w", err)})
	}
	var prior []byte
	if found {
		prior = e.Plan()
	}
	// Even deletion has an uncertain interval after dispatch. Preserve the
	// resolved intent while marking that interval pending.
	if found && (o.e.verb == "delete" || o.e.verb == "exec") {
		proposed = e.Plan()
	}
	if err := d.run.ledger.Begin(ctx, k, proposed, v); err != nil {
		return notAttempted(err)
	}
	form := semantics.FormOf(o.e.verb)
	if m, ok := d.resource.(managed); ok && m.creates[o.e.alias] {
		form = facade.FormCreate
	}
	var about []byte
	if m, ok := d.resource.(managed); ok {
		about = m.about
	}
	if about == nil && found && o.e.verb == "exec" {
		about = e.About()
	}
	if about == nil && d.provider != "" {
		about = d.aboutOf(o.bound)
	}
	if _, err := d.run.journal.AppendAbout(ctx, k, o.e.exchange, form, prior, about); err != nil {
		return notAttempted(err)
	}
	// An object named by the caller is addressed by that name: it is its identity wherever the
	// response reports none.
	named := ""
	if a := d.resource.AddressedBy(); a != "" && o.bound[a] != nil {
		named = fmt.Sprint(o.bound[a])
	}
	ctx = docx.WithAwaitCheckpoint(ctx, func(ctx context.Context, method string, inputs map[string]any) error {
		return d.run.journal.AppendAwait(ctx, k, method, inputs)
	})
	if d.run.held != nil {
		if err := d.run.held.check(ctx); err != nil {
			return failed(err)
		}
	}
	if err := journal.RecordDiagnostic(ctx, d.run.journal, k, o.e.exchange, journal.Diagnostic{Event: "dispatch", Stage: "effect", Outcome: "attempting"}); err != nil {
		return notAttempted(err)
	}
	transferred = true
	return &durableRecords{unlock: unlock, inner: effectRecords{Records: o.inner.Open(retryNever(ctx)), op: o}, d: d, key: k, entry: e, found: found, named: named}
}

type durableRecords struct {
	unlock   func()
	inner    effectRecords
	d        durable
	key      facade.LedgerKey
	entry    facade.LedgerEntry
	found    bool
	first    map[string]any
	identity string
	named    string
	done     bool
	loaded   bool
	records  []facade.Record
	position int
	err      error
}

// A managed effect publishes no row until its complete response has been
// validated and its identity/intent committed. Otherwise downstream mutations
// can race the pending ledger entry or failures can miss compensating the create.
func (r *durableRecords) Next(ctx context.Context) bool {
	if !r.loaded {
		if r.unlock != nil {
			defer r.unlock()
		}
		r.loaded = true
		for r.inner.Next(ctx) {
			record := r.inner.Record()
			r.records = append(r.records, record)
			if r.first == nil {
				r.first, _ = bind.DocMap(record)
				r.identity = r.identityOf()
			}
		}
		r.err = r.inner.Err()
		if r.err == nil && ctx.Err() != nil {
			r.err = &EffectError{Outcome: ErrOutcomeUnknown, Verb: r.inner.op.e.verb, Alias: r.inner.op.e.alias, Cause: ctx.Err()}
		}
		if r.err != nil {
			if errors.Is(r.err, ErrRejected) {
				if err := r.restoreRejected(ctx); err != nil {
					r.err = errors.Join(r.err, err)
				}
			}
			r.recordOutcome(ctx)
			return false
		}
		r.err = r.commit(ctx)
		r.recordOutcome(ctx)
	}
	if r.err != nil || r.position >= len(r.records) {
		return false
	}
	r.position++
	return true
}

// A definite rejection did not change the remote resource. Restore the prior
// resolved state; an unknown response must never take this path.
func (r *durableRecords) restoreRejected(ctx context.Context) error {
	_, v, found, err := r.d.run.ledger.Get(ctx, r.key)
	if err != nil || !found {
		return err
	}
	if !r.found {
		return r.d.run.ledger.Forget(ctx, r.key, v)
	}
	if err := r.d.run.ledger.Begin(ctx, r.key, r.entry.Plan(), v); err != nil {
		return err
	}
	_, v, _, err = r.d.run.ledger.Get(ctx, r.key)
	if err != nil {
		return err
	}
	if err := r.d.run.ledger.Resolve(ctx, r.key, r.entry.Identity(), r.entry.About(), v); err != nil {
		return err
	}
	_, v, _, err = r.d.run.ledger.Get(ctx, r.key)
	if err != nil {
		return err
	}
	r.d.run.mu.Lock()
	defer r.d.run.mu.Unlock()
	for i := range r.d.run.applied {
		if r.d.run.applied[i].key == r.key {
			r.d.run.applied[i].version = v
		}
	}
	return nil
}

// commit records what landed: a create's identity, an update's new intent, a delete's absence.
func (r *durableRecords) commit(ctx context.Context) error {
	if r.d.run.held != nil {
		if err := r.d.run.held.check(ctx); err != nil {
			return err
		}
	}

	identity := r.identity
	if r.first == nil {
		identity = r.identityOf()
	}
	_, ver, found, err := r.d.run.ledger.Get(ctx, r.key)
	if err != nil {
		return err
	}
	op := r.inner.op
	if op.e.verb == "insert" && identity == "" {
		return &EffectError{Outcome: ErrOutcomeUnknown, Verb: op.e.verb, Alias: op.e.alias, Cause: fmt.Errorf("accepted create returned no resource identity; intent remains pending")}
	}
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
		var about []byte
		if m, ok := r.d.resource.(managed); ok {
			about = m.about
		}
		if about == nil && r.found && op.e.verb == "exec" {
			about = r.entry.About()
		}
		if about == nil && r.d.provider != "" {
			about = r.d.aboutOf(r.inner.op.bound)
		}
		if err := r.d.run.ledger.Resolve(ctx, r.key, []byte(identity), about, ver); err != nil {
			return err
		}
	}
	_, committedVersion, _, err := r.d.run.ledger.Get(ctx, r.key)
	if err != nil {
		return err
	}
	r.d.run.mu.Lock()
	r.d.run.applied = append(r.d.run.applied, appliedEffect{key: r.key, resource: r.d.resource, address: r.d.address,
		verb: op.e.verb, identity: identity, version: committedVersion})
	r.d.run.mu.Unlock()
	return nil
}

// identityOf is the resource's identity after this mutation: what its response reports, else what
// the ledger already held.
func (r *durableRecords) identityOf() string {
	identity := r.named
	if r.found && len(r.entry.Identity()) > 0 {
		identity = string(r.entry.Identity())
	}
	v, ok := r.first[r.d.resource.Identity()]
	if m, lowered := r.d.resource.(managed); lowered && m.identityPath != "" {
		v, ok = atPath(r.first, m.identityPath)
	}
	if ok && v != nil && fmt.Sprint(v) != "" {
		identity = fmt.Sprint(v)
	}
	return identity
}

// Record is the mutation's row with the resource's identity beside it.
func (r *durableRecords) Record() facade.Record {
	rec := r.records[r.position-1]
	m, ok := bind.DocMap(rec)
	if !ok {
		return rec
	}
	out := maps.Clone(m)
	out[identityAttr] = r.identity
	return bind.NewDocRecord(out)
}

// atPath walks a dotted path into a decoded response. A list of one is stepped through: a projected
// response wraps the object a call concerns in a row list.
func atPath(doc map[string]any, path string) (any, bool) {
	var cur any = doc
	for _, step := range strings.Split(path, ".") {
		if list, isList := cur.([]any); isList && len(list) == 1 {
			cur = list[0]
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[step]; !ok {
			return nil, false
		}
	}
	if list, isList := cur.([]any); isList && len(list) == 1 {
		cur = list[0]
	}
	return cur, true
}
func (r *durableRecords) Err() error { return r.err }
func (r *durableRecords) Close() error {
	err := r.inner.Close()
	if r.unlock != nil {
		r.unlock()
	}
	return err
}

// ConvergeGraph runs g as an IaC run over collection name, its ledger and journals under state. Every
// mutation node must belong to one of managed. Its resource footprint is held; intent is recorded
// before every request. A failed run preserves partial progress by default.
// Args.IACRollback opts into compensation under retained mutation-target locks;
// a create is deleted by its resource's AddressedBy, reporting what could not reverse. Opening the plan performs the run;
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
		if m.Key() == "" || strings.ContainsAny(m.Key(), "\\\x00") || slices.ContainsFunc(strings.Split(m.Key(), "/"), func(segment string) bool { return segment == "" || strings.HasPrefix(segment, ".") }) {
			return nil, fmt.Errorf("omnisdk: invalid managed resource key %q", m.Key())
		}
		for _, a := range m.Aliases() {
			if other, twice := nodes[a]; twice {
				return nil, fmt.Errorf("omnisdk: %q belongs to both %q and %q", a, other.Key(), m.Key())
			}
			nodes[a] = m
		}
	}

	var blockers []error
	for _, n := range g.Nodes() {
		_, isManaged := nodes[n.Alias()]
		mutating := n.Verb() != "select" && n.Verb() != verbRecall && n.Verb() != verbDiff && n.Verb() != verbShape && n.Verb() != verbFunction
		if mutating && !isManaged {
			blockers = append(blockers, fmt.Errorf("%s %s (%s) belongs to no managed resource: declare whether to manage it or do not perform this update", n.Verb(), n.Alias(), n.Address()))
		}
		if isManaged && !mutating {
			blockers = append(blockers, fmt.Errorf("%q is managed but does not mutate", n.Alias()))
		}
		if mutating && args.Relations[n.Address()] != nil {
			blockers = append(blockers, fmt.Errorf("%s is a read-only relation and cannot be mutated", n.Address()))
		}
	}
	for _, a := range slices.Sorted(maps.Keys(nodes)) {
		if !slices.ContainsFunc(g.Nodes(), func(n Node) bool { return n.Alias() == a }) {
			blockers = append(blockers, fmt.Errorf("managed node %q is not in the graph", a))
		}
	}
	if len(blockers) > 0 {
		return nil, fmt.Errorf("omnisdk: invalid managed graph: %w", errors.Join(blockers...))
	}

	// Planned now, against no state, so a graph that cannot run is refused before anything is held.
	probe := args
	probe.managed = &managedRun{name: name, nodes: nodes}
	if _, err := NewGraphQuery(registry, g, probe); err != nil {
		return nil, err
	}
	if runID == "" {
		runID = time.Now().UTC().Format("20060102T150405Z") + "-" + rand.Text()
	}
	return &convergeGraphPlan{registry: registry, name: name, state: state, runID: runID, g: g, nodes: nodes, args: args}, nil
}

type convergeGraphPlan struct {
	registry, name, state, runID string
	g                            Graph
	nodes                        map[string]Managed
	args                         Args
}

// held is a converging run's durable state, open and leased: the ledger, the run's journal, and the
// resource footprint held until release.
type held struct {
	log      facade.Ledger
	journals facade.Journals
	journal  facade.Journal
	release  func()
	lease    facade.Lease
	ctx      context.Context
}

// hold opens collection name's ledger and journals under state, and holds the collection for runID.
func hold(ctx context.Context, state, name, runID string, footprint ...facade.KeySet) (*held, error) {
	log, err := ledger.NewFile(filepath.Join(state, "ledger"))
	if err != nil {
		return nil, err
	}
	journals, err := journal.NewFiles(filepath.Join(state, "journal"))
	if err != nil {
		return nil, err
	}
	keys := lease.All()
	if len(footprint) > 0 {
		keys = footprint[0]
	}
	l, err := lease.NewLeaser(log, time.Now).Acquire(ctx, name, keys, runID, leaseTTL)
	if errors.Is(err, facade.ErrLeaseHeld) {
		// How runs are kept apart is not the caller's business; that another is running is.
		return nil, fmt.Errorf("omnisdk: collection %q is busy with another run", name)
	}
	if err != nil {
		return nil, err
	}
	j, err := journals.Create(ctx, runID)
	if err != nil {
		_ = l.Release(ctx)
		return nil, err
	}
	h := &held{log: log, journals: journals, journal: j, lease: l}
	h.maintain(ctx, leaseTTL)
	return h, nil
}

func (p *convergeGraphPlan) Open(ctx context.Context) (Rows, error) {
	var accessErr error
	ctx, accessErr = authorizeExecution(ctx, p.args, facade.Permission{Resource: "collection/" + p.name, Action: "converge"})
	if accessErr != nil {
		return nil, accessErr
	}
	recoveryCtx := ctx
	locks := graphLocks(p.g, p.nodes, p.name)
	h, err := hold(ctx, p.state, p.name, p.runID, locks.footprint())
	if err != nil {
		return nil, err
	}
	defer h.release()
	ctx = h.ctx
	locks.h = h
	run := &managedRun{name: p.name, ledger: h.log, journal: h.journal, nodes: p.nodes, held: h}
	if !p.args.rollbackEnabled() {
		run.locks = locks
	}
	a := p.args
	a.managed = run
	pl, err := NewGraphQuery(p.registry, p.g, a)
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
	if runErr == nil {
		runErr = ctx.Err()
	}
	rows.Close()
	if runErr != nil {
		runErr = recordRunFailure(ctx, h, "", runErr)
	}
	if runErr == nil {
		return &sliceRows{rows: out, i: -1}, nil
	}
	var compensated, completed []facade.LedgerKey
	run.mu.Lock()
	for _, effect := range run.applied {
		completed = append(completed, effect.key)
	}
	run.mu.Unlock()
	status := "disabled"
	var rollbackErr error
	if p.args.rollbackEnabled() {
		recovery, err := beginRollback(recoveryCtx, h, p.name, p.runID)
		if err != nil {
			rollbackErr = err
			status = "outstanding"
		} else {
			defer recovery.release()
			run.held = recovery
			compensated, completed = p.compensate(recovery.ctx, run)
			status = "completed"
			if len(completed) > 0 {
				status = "outstanding"
			}
		}
	}
	failure := Row{"status": "failed", "error": runErr.Error(), "rollback": status,
		"compensated": keyStrings(compensated), "completed": keyStrings(completed)}
	if rollbackErr != nil {
		failure["rollback_error"] = rollbackErr.Error()
	}
	out = append(out, failure)
	return &sliceRows{rows: out, i: -1}, nil
}

// compensate reverses what the run did, latest first: a create is deleted through its resource's
// document, addressed by the identity it minted. A provider refusing a delete — a dependent still
// there — is retried on the next pass, until a pass makes no progress. An update or delete is not
// reversed: it completed, and is reported so.
func (p *convergeGraphPlan) compensate(ctx context.Context, run *managedRun) (done, left []facade.LedgerKey) {
	// Caller retained exclusive mutation locks from before the forward run.
	if run.held == nil {
		return nil, appliedKeys(run)
	}
	for _, key := range appliedKeys(run) {
		if !lease.Exclusive(run.held.lease.Keys(), key) {
			return nil, appliedKeys(run)
		}
	}
	run.mu.Lock()
	pending := slices.Clone(run.applied)
	run.mu.Unlock()
	entries, err := run.ledger.List(ctx, run.name+"/")
	if err != nil {
		for _, e := range pending {
			left = append(left, e.key)
		}
		return nil, left
	}
	for _, entry := range entries {
		if entry.Phase() == facade.LedgerPending && run.held.lease.Keys().Contains(entry.Key()) && !strings.HasPrefix(strings.TrimPrefix(string(entry.Key()), run.name+"/"), ".") {
			for _, e := range pending {
				left = append(left, e.key)
			}
			left = append(left, entry.Key())
			return nil, left
		}
	}
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
			entries, listErr := run.ledger.List(ctx, run.name+"/")
			uncertain := listErr != nil
			for _, entry := range entries {
				if entry.Phase() == facade.LedgerPending && run.held.lease.Keys().Contains(entry.Key()) && !strings.HasPrefix(strings.TrimPrefix(string(entry.Key()), run.name+"/"), ".") {
					uncertain = true
				}
			}
			// Do not remove a parent while any retained resource still refers to it,
			// including dependents created by another run before rollback began.
			for _, entry := range entries {
				if entry.Key() == e.key {
					continue
				}
				var about aboutRecord
				if json.Unmarshal(entry.About(), &about) == nil && slices.Contains(about.DependsOn, string(e.key)) {
					uncertain = true
				}
			}
			if uncertain {
				next = append(next, e)
				continue
			}
			_, v, found, err := run.ledger.Get(ctx, e.key)
			if err != nil || !found || v != e.version {
				next = append(next, e)
				continue
			}
			if run.held != nil {
				if err := run.held.check(ctx); err != nil {
					next = append(next, e)
					continue
				}
			}
			undo.managed = &managedRun{name: run.name, ledger: run.ledger, journal: run.journal, held: run.held, nodes: map[string]Managed{"undo": e.resource}}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	g, err := NewGraph([]Node{NewMutationNode("undo", e.address, "delete",
		map[string]string{e.resource.AddressedBy(): e.identity}, nil, nil)}, nil)
	if err != nil {
		return err
	}
	pl, err := NewGraphQuery(registry, g, args)
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
	if err := rows.Err(); err != nil {
		return err
	}
	return ctx.Err()
}

// retryNever is ctx with retries off: a mutation's request repeated after it reached the provider
// repeats the effect.
func retryNever(ctx context.Context) context.Context {
	return retry.Disable(retry.WithPolicy(ctx, never{}))
}

// failedOp is an operator whose run fails with err.
type failedOp struct{ err error }

func (f failedOp) Open(context.Context) facade.Records { return failed(f.err) }

// A diff is evaluated when opened, not while binding its factory: the active caller and
// cancellation context must reach route selection before a mutation arm can be enabled.
type diffOperator struct {
	run     *managedRun
	node    intrinsicNode
	fields  []string
	desired []byte
	bound   map[string]any
}

func (o diffOperator) Open(ctx context.Context) facade.Records {
	row, err := diffRow(ctx, o.run, o.node, o.fields, o.desired, o.bound)
	if err != nil {
		return failed(err)
	}
	return staticOp{rows: []map[string]any{row}}.Open(ctx)
}
