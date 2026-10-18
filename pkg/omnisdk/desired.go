package omnisdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/analyze"
	"github.com/stackql-labs/omnisdk/internal/docsem"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/unwind"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

// Desired state is a converging graph written for the caller. Each resource becomes:
//
//	recall → read (outer) → diff → branch ─ absent ─────────────→ create
//	                                      ├ drift, method m₁ ──→ update via m₁
//	                                      ├ …                    …
//	                                      └ otherwise ─────────→ nothing
//
// with one update arm per update method the document declares that the resource's fields could
// reach. Which arm a drift takes is decided by the diff from the fields that differ, so every method
// in the graph is fixed before it runs. A resource reading another's identity is wired from that
// resource's create, which reports the identity it minted, else from its diff, which reports the
// identity it found.

// desiredPlan converges a flat desired state as a graph, lowered under the collection's lease so
// the ledger it reads cannot change while it is read.
type desiredPlan struct {
	registry, name, state, runID string
	resources                    []ManagedResource
	args                         Args
	targets                      map[string]*target
	// effector and semantics undo a failed run through its journal: a create is deleted and an
	// update has its prior intent restored.
	effector  facade.Effector
	semantics facade.Semantics
}

// target is what the documents say about one resource's address.
type target struct {
	cat                    aot.Catalog
	addr                   string
	createBody, updateBody bool
	updates                []facade.Method
}

// unaddressable is an input nothing supplies: a read requiring it never runs.
const unaddressable = "\x00unaddressable"

func newDesiredPlan(registry, name, state, runID string, resources []ManagedResource, args Args) (Plan, error) {
	effector, sem, err := providerWiring(registry, resources, args)
	if err != nil {
		return nil, err
	}
	reg, err := openRegistry(registry)
	if err != nil {
		return nil, err
	}
	p := &desiredPlan{registry: registry, name: name, state: state, runID: runID, resources: resources, args: args,
		targets: map[string]*target{}, effector: effector, semantics: sem}
	seen := map[string]bool{}
	catalogs := map[string]aot.Catalog{}
	for _, r := range resources {
		if seen[r.Key()] {
			return nil, fmt.Errorf("omnisdk: %s appears twice", r.Key())
		}
		seen[r.Key()] = true
		if typ, _ := r.Via(); typ != "" {
			return nil, fmt.Errorf("omnisdk: %s declares an inbound transform, which a converging graph cannot place: it does not say which parameters the transform provides", r.Key())
		}
		cat, ok := catalogs[r.Provider()]
		if !ok {
			if cat, err = reg.Catalog(r.Provider()); err != nil {
				return nil, fmt.Errorf("omnisdk: provider %q: %w", r.Provider(), err)
			}
			catalogs[r.Provider()] = cat
		}
		addr, err := qualify(cat, r.Address())
		if err != nil {
			return nil, err
		}
		t := &target{cat: cat, addr: addr, createBody: takesBody(cat, addr, "insert"), updateBody: takesBody(cat, addr, "update")}
		if ms, err := docsem.Capabilities(cat).Methods(addr, "update"); err == nil {
			t.updates = ms
		}
		p.targets[r.Key()] = t
	}
	// Lowered now against no prior state, so a desired state that cannot run is refused before
	// anything is held.
	g, nodes, _, err := p.lower(func(string) map[string]any { return nil }, func(string) (string, error) { return "", nil })
	if err != nil {
		return nil, err
	}
	probe := args
	probe.managed = &managedRun{name: name, nodes: nodes}
	if _, err := NewGraphQuery(registry, g, probe); err != nil {
		return nil, err
	}
	return p, nil
}

// takesBody reports whether the operations for verb on addr take a request body. Every operation for
// one verb on one resource agrees: it is a property of the API's style, not of the arguments.
func takesBody(cat aot.Catalog, addr, verb string) bool {
	ops, err := cat.Operations(addr, verb)
	return err == nil && len(ops) > 0 && ops[0].Request().BodyMediaType() != ""
}

func (p *desiredPlan) Open(ctx context.Context) (Rows, error) {
	h, err := hold(ctx, p.state, p.name, p.runID)
	if err != nil {
		return nil, err
	}
	defer h.release()
	key := func(k string) facade.LedgerKey { return facade.LedgerKey(p.name + "/" + k) }
	prior := func(k string) map[string]any {
		e, _, found, err := h.log.Get(ctx, key(k))
		if err != nil || !found {
			return nil
		}
		m, _ := decodeObject(e.Plan())
		return m
	}
	// A resource read from outside the run must already be live: its identity is what it was.
	outside := func(k string) (string, error) {
		e, _, found, err := h.log.Get(ctx, key(k))
		if err != nil {
			return "", err
		}
		if !found || e.Phase() != facade.LedgerLive {
			return "", fmt.Errorf("omnisdk: a resource reads from %s, which is not live", key(k))
		}
		return string(e.Identity()), nil
	}
	g, nodes, owners, err := p.lower(prior, outside)
	if err != nil {
		return nil, err
	}
	run := &managedRun{name: p.name, ledger: h.log, journal: h.journal, nodes: nodes}
	a := p.args
	a.managed = run
	pl, err := NewGraphQuery(p.registry, g, a)
	if err != nil {
		return nil, err
	}
	rows, err := pl.Open(ctx)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
	}
	runErr := rows.Err()
	rows.Close()

	run.mu.Lock()
	var landed []facade.LedgerKey
	for _, e := range run.applied {
		if !slices.Contains(landed, e.key) {
			landed = append(landed, e.key)
		}
	}
	run.mu.Unlock()
	if runErr == nil {
		var all []facade.LedgerKey
		for _, r := range p.resources {
			all = append(all, key(r.Key()))
		}
		return reportRows(ctx, h.log, all, nil, "", nil), nil
	}
	// Forward recovery is preferred where it is reachable, because compensation is lossy — billing
	// events, sent notifications, consumed ids and deleted data do not come back. Choosing between
	// them is the caller's policy, so the outcome is reported rather than decided here.
	out, unwindErr := unwind.New(h.log, h.journals, p.semantics, p.effector).Unwind(ctx, p.runID)
	if unwindErr != nil {
		return nil, errors.Join(runErr, unwindErr)
	}
	return reportRows(ctx, h.log, landed, &out, failedKey(runErr, owners, p.name), runErr), nil
}

// failedKey is the resource a run's failure belongs to: the node it names, through owners.
func failedKey(err error, owners map[string]string, collection string) facade.LedgerKey {
	var effect *EffectError
	if errors.As(err, &effect) {
		if k, ok := owners[effect.Alias]; ok {
			return facade.LedgerKey(collection + "/" + k)
		}
	}
	for alias, k := range owners {
		if strings.Contains(err.Error(), alias) {
			return facade.LedgerKey(collection + "/" + k)
		}
	}
	return ""
}

// reportRows is one row per key, and a final row when the run failed, so a partial compensation is
// visible rather than implied by an absence. A key applied and then compensated must not read as
// "applied": the run left nothing behind, and reporting otherwise sends someone looking for a
// resource that is gone.
func reportRows(ctx context.Context, log facade.Ledger, keys []facade.LedgerKey, unwound *unwind.Outcome,
	failed facade.LedgerKey, runErr error) Rows {
	undone := map[facade.LedgerKey]bool{}
	if unwound != nil {
		for _, k := range unwound.Compensated {
			undone[k] = true
		}
	}
	var out []Row
	for _, k := range keys {
		row := Row{"key": string(k), "status": "applied"}
		if undone[k] {
			row["status"] = "applied, then compensated"
		}
		if e, _, found, err := log.Get(ctx, k); err == nil && found {
			row["identity"] = string(e.Identity())
		}
		out = append(out, row)
	}
	if runErr != nil {
		row := Row{"key": string(failed), "status": "failed", "error": runErr.Error()}
		if unwound != nil {
			row["compensated"] = keyStrings(unwound.Compensated)
			row["outstanding"] = keyStrings(unwound.Outstanding)
			if !unwound.Complete() {
				row["status"] = "failed; partially compensated"
			}
		}
		out = append(out, row)
	}
	return &sliceRows{rows: out, i: -1}
}

// lower writes the desired state as a converging graph. prior is the intent the ledger last recorded
// for a resource, whose fields are unset when desired drops them; outside is the identity of a
// resource read from but not in the run. owners maps each node to its resource's key.
func (p *desiredPlan) lower(prior func(string) map[string]any, outside func(string) (string, error)) (Graph, map[string]Managed, map[string]string, error) {
	index := map[string]int{}
	for i, r := range p.resources {
		index[r.Key()] = i
	}
	base := func(i int) string { return fmt.Sprintf("r%d", i) }
	var nodes []Node
	var wirings []Wiring
	type branched struct {
		b     Branch
		gates []Gate
	}
	var branches []branched
	managedNodes := map[string]Managed{}
	owners := map[string]string{}

	for i, r := range p.resources {
		t := p.targets[r.Key()]
		b := base(i)
		recall, live, diff, create := b+"_r", b+"_l", b+"_d", b+"_c"
		desired, err := decodeObject(r.Desired())
		if err != nil {
			return nil, nil, nil, fmt.Errorf("omnisdk: %s: desired state is not a JSON object: %w", r.Key(), err)
		}
		fields := slices.Sorted(maps.Keys(desired))
		// A field the last run asked for and this one does not is unset: the diff reads it live and
		// sends its removal, so it is part of what the diff and the update see.
		want := maps.Clone(desired)
		for k := range prior(r.Key()) {
			if _, kept := want[k]; !kept {
				want[k] = nil
			}
		}
		all := slices.Sorted(maps.Keys(want))

		// What the resource reads from others: an identity from a resource in the run arrives over
		// edges; one from outside it was live before the run, and is a constant.
		var arrivals []Inbound
		consts := map[string]string{}
		for _, in := range r.Inbound() {
			as := in.As
			if as == "" {
				as = in.From
			}
			if j, ok := index[in.From]; ok {
				arrivals = append(arrivals, NewInbound(base(j)+"_c", identityAttr, as), NewInbound(base(j)+"_d", "identity", as))
				continue
			}
			id, err := outside(in.From)
			if err != nil {
				return nil, nil, nil, err
			}
			consts[as] = id
		}

		liveIdentity := r.AddressedBy()
		if path := r.Identity(); path != "" {
			liveIdentity = path[strings.LastIndex(path, ".")+1:]
		}
		requires := []string{r.AddressedBy()}
		if r.AddressedBy() == "" {
			requires = []string{unaddressable}
		}
		nodes = append(nodes,
			NewRecallNode(recall, r.Key()),
			node{alias: live, address: t.addr, verb: "select", outer: true, requires: requires})
		if r.AddressedBy() != "" {
			wirings = append(wirings, NewWiring(live, []Inbound{NewInbound(recall, "identity", r.AddressedBy())}, "", ""))
		}
		d := NewDiffNode(diff, r.Key(), live, liveIdentity, want).(intrinsicNode)
		d.route = p.route(r, t, all)
		nodes = append(nodes, d)

		createParams := maps.Clone(r.Params())
		if createParams == nil {
			createParams = map[string]string{}
		}
		maps.Copy(createParams, consts)
		if c := r.CorrelationParam(); c != "" {
			createParams[c] = p.name + "/" + r.Key()
		}
		c := node{alias: create, address: t.addr, verb: "insert", params: createParams, whole: true}
		if t.createBody {
			c.body = nilFields(fields)
		}
		nodes = append(nodes, c)
		createIn := slices.Clone(arrivals)
		for _, f := range fields {
			createIn = append(createIn, NewInbound(diff, f, f))
		}
		wirings = append(wirings, NewWiring(create, createIn, "", ""))

		arms := []Arm{NewArm("create", query.NewEq(query.NewColumn(diff, "status"), query.NewLiteral(DiffAbsent)))}
		gates := []Gate{NewGate(b+"_b", "create", create)}
		aliases := []string{create}
		for j, m := range p.reachable(r, t, all, consts) {
			u := fmt.Sprintf("%s_u%d", b, j)
			un := node{alias: u, address: t.addr, verb: "update", params: maps.Clone(consts), pin: m.Name(), whole: true}
			if t.updateBody {
				un.body = nilFields(all)
			}
			nodes = append(nodes, un)
			in := append(slices.Clone(arrivals), NewInbound(diff, "identity", r.AddressedBy()))
			for _, f := range all {
				in = append(in, NewInbound(diff, f, f))
			}
			wirings = append(wirings, NewWiring(u, in, "", ""))
			arms = append(arms, NewArm(u, query.NewEq(query.NewColumn(diff, "update"), query.NewLiteral(m.Name()))))
			gates = append(gates, NewGate(b+"_b", u, u))
			aliases = append(aliases, u)
		}
		br, err := NewBranch(b+"_b", append(arms, Otherwise("keep"))...)
		if err != nil {
			return nil, nil, nil, err
		}
		branches = append(branches, branched{br, gates})
		m := managed{key: r.Key(), aliases: aliases, addressedBy: r.AddressedBy(), identityPath: r.Identity(), intent: r.Desired()}
		for _, a := range aliases {
			managedNodes[a] = m
		}
		for _, a := range append([]string{recall, live, diff}, aliases...) {
			owners[a] = r.Key()
		}
	}
	g, err := NewGraph(nodes, wirings)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, x := range branches {
		if g, err = WithBranch(g, x.b, x.gates...); err != nil {
			return nil, nil, nil, err
		}
	}
	return g, managedNodes, owners, nil
}

// reachable are the update methods a drift on r could take: those whose required inputs the
// resource's fields, its address, the run's scope and what it reads from others can supply. One
// needing something none of them can is never chosen, and is left out of the graph.
func (p *desiredPlan) reachable(r ManagedResource, t *target, fields []string, consts map[string]string) []facade.Method {
	have := map[string]bool{r.AddressedBy(): true}
	for _, f := range fields {
		have[f] = true
	}
	for k := range p.args.Params {
		have[k] = true
	}
	for k := range consts {
		have[k] = true
	}
	for _, in := range r.Inbound() {
		as := in.As
		if as == "" {
			as = in.From
		}
		have[as] = true
	}
	var out []facade.Method
	for _, m := range t.updates {
		if !slices.ContainsFunc(m.Required(), func(req string) bool { return !have[req] }) {
			out = append(out, m)
		}
	}
	return out
}

// route chooses the update for a drift on r. An operation taking a body is sent the whole mutation:
// a body may replace the object, and a partial one would erase every field it leaves out. One
// without a body is sent only the fields that differ, since each is a parameter it must declare, and
// cannot express removing a field, so a field to unset is refused. The method is the one that
// accepts the object's address and everything sent, chosen by the same analysis as any statement.
func (p *desiredPlan) route(r ManagedResource, t *target, fields []string) driftRoute {
	an := analyze.New(docsem.Capabilities(t.cat), analyze.PermitAll())
	return func(mutation, live map[string]any) (string, map[string]any, error) {
		send := mutation
		if !t.updateBody {
			send = map[string]any{}
			for k, v := range mutation {
				got, present := live[k]
				switch {
				case v == nil && present:
					return "", nil, fmt.Errorf("%s is to be removed, and an operation without a body cannot remove a field", k)
				case v == nil:
				case !present || fmt.Sprint(v) != fmt.Sprint(got):
					send[k] = v
				}
			}
		}
		sent := append(slices.Sorted(maps.Keys(send)), r.AddressedBy())
		available := append(slices.Clone(sent), slices.Collect(maps.Keys(p.args.Params))...)
		m, err := an.Analyze(context.Background(), analyze.NewStatement(principalOf(p.args), t.addr, "update", available, sent, ""))
		if err != nil {
			return "", nil, err
		}
		return m.Name(), send, nil
	}
}

func nilFields(names []string) map[string]any {
	out := make(map[string]any, len(names))
	for _, n := range names {
		out[n] = nil
	}
	return out
}

// decodeObject reads a JSON object keeping numbers as their source text.
func decodeObject(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}
