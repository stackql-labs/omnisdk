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
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/unwind"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/schemaxml"
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
	// createVerb and updateVerb are the document's verbs for making the object and for changing
	// it: insert and update, or replace where a PUT is the only write it declares.
	createVerb, updateVerb string
	updates                []facade.Method
	// read is how the object is read back: a get addressed by its identity, or — where the document
	// has none — a list whose rows are matched on the identity column. readParams are the resource's
	// own parameters the read takes, a resource group, say.
	read       facade.Method
	listed     bool
	readParams map[string]string
	// incoming are the inputs that reach the resource from others: what arrives, or — through an
	// inbound transform — what the transform provides.
	incoming []string
}

// incomingOf names what reaches r from other resources. Without a transform it is each arrival
// under its name; with one it is the transform's output, found by running the program once on
// placeholder values — a program's keys are its shape, not its data. One whose keys depend on the
// values is not covered.
func incomingOf(r ManagedResource) ([]string, error) {
	var inbox []string
	for _, in := range r.Inbound() {
		as := in.As
		if as == "" {
			as = in.From
		}
		inbox = append(inbox, as)
	}
	typ, body := r.Via()
	if typ == "" {
		return inbox, nil
	}
	reg, err := dsl.NewRegistry(append(gotemplate.Evaluators(), schemaxml.New())...)
	if err != nil {
		return nil, err
	}
	sample := map[string]any{}
	for _, in := range inbox {
		sample[in] = "placeholder"
	}
	shaped, err := docx.InboundProgram(reg, typ, body).Apply(bind.NewDocRecord(sample))
	if err != nil {
		return nil, fmt.Errorf("omnisdk: %s: inbound transform: %w", r.Key(), err)
	}
	out, ok := bind.DocMap(shaped)
	if !ok {
		return nil, fmt.Errorf("omnisdk: %s: inbound transform did not yield a document", r.Key())
	}
	return slices.Sorted(maps.Keys(out)), nil
}

// unaddressable is an input nothing supplies: a read requiring it never runs.
const unaddressable = "\x00unaddressable"

func newDesiredPlan(registry, name, state, runID string, resources []ManagedResource, args Args) (Plan, error) {
	// What the caller wrote is checked before anything is looked up on their behalf: an empty value
	// is a value nobody gave — an unset environment variable, most often — and would otherwise
	// surface far from its cause, as a credential or a method that cannot be found.
	for _, r := range resources {
		for _, k := range slices.Sorted(maps.Keys(r.Params())) {
			if r.Params()[k] == "" {
				return nil, fmt.Errorf("omnisdk: %s: parameter %q is empty", r.Key(), k)
			}
		}
	}
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
		incoming, err := incomingOf(r)
		if err != nil {
			return nil, err
		}
		t, err := targetOf(cat, addr, r, args, incoming)
		if err != nil {
			return nil, err
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

// targetOf is what the documents say of r's address: the verbs that make and change it, and how it
// is read back.
func targetOf(cat aot.Catalog, addr string, r ManagedResource, args Args, incoming []string) (*target, error) {
	caps := docsem.Capabilities(cat)
	t := &target{cat: cat, addr: addr, incoming: incoming}
	for _, v := range []string{"insert", "replace"} {
		if ms, err := caps.Methods(addr, v); err == nil && len(ms) > 0 {
			t.createVerb = v
			break
		}
	}
	if t.createVerb == "" {
		return nil, fmt.Errorf("omnisdk: %s: %s declares no insert or replace, so nothing can make it", r.Key(), addr)
	}
	for _, v := range []string{"update", "replace"} {
		if ms, err := caps.Methods(addr, v); err == nil && len(ms) > 0 {
			t.updateVerb, t.updates = v, ms
			break
		}
	}
	t.createBody, t.updateBody = takesBody(cat, addr, t.createVerb), takesBody(cat, addr, t.updateVerb)

	// The read: a GET is all that may read an object back — a POST bound to select, such as a key
	// listing, is a call with effects of its own. Addressed by the identity where one takes it,
	// else a list the identity is found in. The most specific satisfiable wins either way.
	have := map[string]bool{}
	for k := range args.Params {
		have[k] = true
	}
	for k := range r.Params() {
		have[k] = true
	}
	// What it reads from other resources reaches the read too: a container is read within its
	// account.
	for _, in := range incoming {
		have[in] = true
	}
	selects, _ := caps.Methods(addr, "select")
	pick := func(addressed bool) facade.Method {
		var best facade.Method
		for _, m := range selects {
			ex, ok := docsem.Exchange(m)
			if !ok || ex.Request().Method() != "GET" {
				continue
			}
			takes := r.AddressedBy() != "" && m.Accepts(r.AddressedBy())
			if addressed != takes {
				continue
			}
			if slices.ContainsFunc(m.Required(), func(q string) bool { return !have[q] && !(addressed && q == r.AddressedBy()) }) {
				continue
			}
			if best == nil || len(m.Required()) > len(best.Required()) {
				best = m
			}
		}
		return best
	}
	if t.read = pick(true); t.read == nil {
		t.read, t.listed = pick(false), true
	}
	if t.read != nil {
		t.readParams = map[string]string{}
		for k, v := range r.Params() {
			if k != r.AddressedBy() && declares(t.read, k) {
				t.readParams[k] = v
			}
		}
	}
	return t, nil
}

// takesBody reports whether the operations for verb on addr take a request body. Every operation for
// one verb on one resource agrees: it is a property of the API's style, not of the arguments.
func takesBody(cat aot.Catalog, addr, verb string) bool {
	ops, err := cat.Operations(addr, verb)
	return err == nil && len(ops) > 0 && ops[0].Request().BodyMediaType() != ""
}

func (p *desiredPlan) Open(ctx context.Context) (Rows, error) {
	var accessErr error
	ctx, accessErr = executionContext(ctx, p.args)
	if accessErr != nil {
		return nil, accessErr
	}
	if err := facade.CheckAccess(ctx, "collection/"+p.name, "converge"); err != nil {
		return nil, err
	}
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
	out, unwindErr := unwind.New(h.log, h.journals, p.semantics, scoped{inner: p.effector, scope: p.args.Params, log: h.log}).Unwind(ctx, p.runID)
	if unwindErr != nil {
		return nil, errors.Join(runErr, unwindErr)
	}
	return reportRows(ctx, h.log, landed, &out, failedKey(runErr, owners, p.name), runErr), nil
}

// scoped gives an undo's calls what the run's own calls had: the run's scope — the region a call is
// signed into, say — and the values the ledger records the key's delete takes, a resource group. A
// journal records what was done, not the scope it was done in.
type scoped struct {
	inner facade.Effector
	scope map[string]string
	log   facade.Ledger
}

func (s scoped) with(ctx context.Context, k facade.LedgerKey, in facade.EffectInput) facade.EffectInput {
	params := maps.Clone(s.scope)
	if params == nil {
		params = map[string]string{}
	}
	if e, _, found, err := s.log.Get(ctx, k); err == nil && found {
		var rec aboutRecord
		if json.Unmarshal(e.About(), &rec) == nil {
			maps.Copy(params, rec.inputs(ctx, s.log))
		}
	}
	maps.Copy(params, in.Params)
	in.Params = params
	return in
}

func (s scoped) Effect(ctx context.Context, exchange string, k facade.LedgerKey, in facade.EffectInput) ([]byte, error) {
	return s.inner.Effect(ctx, exchange, k, s.with(ctx, k, in))
}

func (s scoped) Read(ctx context.Context, exchange string, k facade.LedgerKey, in facade.EffectInput) ([]byte, []byte, bool, error) {
	return s.inner.Read(ctx, exchange, k, s.with(ctx, k, in))
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
		if strings.Contains(err.Error(), alias) || strings.Contains(err.Error(), k) {
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
			if len(unwound.Reasons) > 0 {
				why := map[string]string{}
				for k, v := range unwound.Reasons {
					why[string(k)] = v
				}
				row["reasons"] = why
			}
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

		// An inbound transform reshapes what arrives into the parameters the calls take: it is a
		// node of its own between the arrivals and every call, as T_in is on any wiring.
		if typ, body := r.Via(); typ != "" {
			if len(consts) > 0 {
				return nil, nil, nil, fmt.Errorf("omnisdk: %s: an inbound transform over a resource outside the run is not supported", r.Key())
			}
			shape := b + "_s"
			var inbox []string
			for _, in := range r.Inbound() {
				if in.As != "" {
					inbox = append(inbox, in.As)
				} else {
					inbox = append(inbox, in.From)
				}
			}
			nodes = append(nodes, intrinsicNode{node: node{alias: shape, verb: verbShape}, inbox: inbox,
				viaType: typ, viaBody: body, provides: t.incoming})
			if len(arrivals) > 0 {
				wirings = append(wirings, NewWiring(shape, arrivals, "", ""))
			}
			arrivals = nil
			for _, name := range t.incoming {
				arrivals = append(arrivals, NewInbound(shape, name, name))
			}
			owners[shape] = r.Key()
		}

		liveIdentity := r.AddressedBy()
		if path := r.Identity(); path != "" {
			liveIdentity = path[strings.LastIndex(path, ".")+1:]
		}
		// The read runs only once there is an identity to read by: a resource never made is absent,
		// and a read without its address would list everything.
		ln := node{alias: live, address: t.addr, verb: "select", outer: true, params: maps.Clone(t.readParams)}
		maps.Copy(ln.params, consts)
		// The read is addressed only by the identity the ledger recorded, never by a value arriving
		// from another resource, or it would find an object this run did not make.
		readIn := slices.DeleteFunc(slices.Clone(arrivals), func(in Inbound) bool { return in.As() == r.AddressedBy() })
		delete(ln.params, r.AddressedBy())
		switch {
		case t.read == nil || (r.AddressedBy() == "" && !t.listed):
			ln.requires = []string{unaddressable}
		case t.listed:
			// No get: the object is the listed row whose identity column holds its identity.
			private := hidden(recall, "identity")
			ln.pin, ln.requires = t.read.Name(), []string{private}
			ln.on = []query.Predicate{query.NewEq(query.NewColumn(live, liveIdentity), query.NewColumn(recall, "identity"))}
			wirings = append(wirings, NewWiring(live, append([]Inbound{NewInbound(recall, "identity", private)}, readIn...), "", ""))
		default:
			ln.pin, ln.requires = t.read.Name(), []string{r.AddressedBy()}
			wirings = append(wirings, NewWiring(live, append([]Inbound{NewInbound(recall, "identity", r.AddressedBy())}, readIn...), "", ""))
		}
		nodes = append(nodes, NewRecallNode(recall, r.Key()), ln)
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
		c := node{alias: create, address: t.addr, verb: t.createVerb, params: createParams, whole: true}
		if t.createBody {
			c.body = nilFields(fields)
		}
		nodes = append(nodes, c)
		createIn := slices.Clone(arrivals)
		for _, f := range fields {
			createIn = append(createIn, NewInbound(diff, f, f))
		}
		if len(createIn) > 0 {
			wirings = append(wirings, NewWiring(create, createIn, "", ""))
		}

		arms := []Arm{NewArm("create", query.NewEq(query.NewColumn(diff, "status"), query.NewLiteral(DiffAbsent)))}
		gates := []Gate{NewGate(b+"_b", "create", create)}
		aliases := []string{create}
		for j, m := range p.reachable(r, t, all, consts) {
			u := fmt.Sprintf("%s_u%d", b, j)
			// The update takes the resource's own parameters it declares — a resource group, say —
			// but not the address, which arrives from the diff, nor anything only the create takes.
			params := maps.Clone(consts)
			for k, v := range r.Params() {
				if k != r.AddressedBy() && declares(m, k) {
					params[k] = v
				}
			}
			un := node{alias: u, address: t.addr, verb: t.updateVerb, params: params, pin: m.Name(), whole: true}
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
		about, err := p.about(r, t)
		if err != nil {
			return nil, nil, nil, err
		}
		m := managed{key: r.Key(), aliases: aliases, addressedBy: r.AddressedBy(), identityPath: r.Identity(), intent: r.Desired(), about: about,
			creates: map[string]bool{create: true}}
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

// aboutRecord is what the ledger records a desired-state resource is: enough to delete it from the
// ledger alone, and to delete what depends on it first.
type aboutRecord struct {
	Provider    string `json:"provider"`
	Address     string `json:"address"`
	AddressedBy string `json:"addressed_by,omitempty"`
	// Params are the resource's own values its delete declares — a resource group, say.
	Params map[string]string `json:"params,omitempty"`
	// DependsOn are the ledger keys this resource reads from.
	DependsOn []string `json:"depends_on,omitempty"`
	// Arrives maps each input that comes from another resource to that resource's ledger key: its
	// recorded identity is the value — a container's account name, say.
	Arrives map[string]string `json:"arrives,omitempty"`
}

// inputs are what a call on a recorded resource takes from the ledger: its own values, and the
// identities of the keys it reads from.
func (rec aboutRecord) inputs(ctx context.Context, log facade.Ledger) map[string]string {
	out := maps.Clone(rec.Params)
	if out == nil {
		out = map[string]string{}
	}
	for as, k := range rec.Arrives {
		if e, _, found, err := log.Get(ctx, facade.LedgerKey(k)); err == nil && found && len(e.Identity()) > 0 {
			out[as] = string(e.Identity())
		}
	}
	// A key this one depends on is addressed by its own identity, under the parameter it is
	// addressed by: an account's identity is its account_name, which a container in it takes too. A
	// record written before arrivals were kept still resolves this way.
	for _, k := range rec.DependsOn {
		e, _, found, err := log.Get(ctx, facade.LedgerKey(k))
		if err != nil || !found || len(e.Identity()) == 0 {
			continue
		}
		var dep aboutRecord
		if json.Unmarshal(e.About(), &dep) != nil || dep.AddressedBy == "" {
			continue
		}
		if _, given := out[dep.AddressedBy]; !given {
			out[dep.AddressedBy] = string(e.Identity())
		}
	}
	return out
}

func (p *desiredPlan) about(r ManagedResource, t *target) ([]byte, error) {
	rec := aboutRecord{Provider: r.Provider(), Address: r.Address(), AddressedBy: r.AddressedBy()}
	deletes, _ := docsem.Capabilities(t.cat).Methods(t.addr, "delete")
	for k, v := range r.Params() {
		if k != r.AddressedBy() && slices.ContainsFunc(deletes, func(m facade.Method) bool { return declares(m, k) }) {
			if rec.Params == nil {
				rec.Params = map[string]string{}
			}
			rec.Params[k] = v
		}
	}
	for _, in := range r.Inbound() {
		rec.DependsOn = append(rec.DependsOn, p.name+"/"+in.From)
		as := in.As
		if as == "" {
			as = in.From
		}
		if rec.Arrives == nil {
			rec.Arrives = map[string]string{}
		}
		rec.Arrives[as] = p.name + "/" + in.From
	}
	return json.Marshal(rec)
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
	for k := range r.Params() {
		have[k] = true
	}
	for k := range consts {
		have[k] = true
	}
	for _, in := range t.incoming {
		have[in] = true
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
		available = append(available, slices.Collect(maps.Keys(r.Params()))...)
		available = append(available, t.incoming...)
		m, err := an.Analyze(context.Background(), analyze.NewStatement(principalOf(p.args), t.addr, t.updateVerb, available, sent, ""))
		if err != nil {
			return "", nil, err
		}
		return m.Name(), send, nil
	}
}

// declares reports whether m's operation names input k among its parameters or server variables —
// not merely accepts it, as a method taking a body accepts any field.
func declares(m facade.Method, k string) bool {
	ex, ok := docsem.Exchange(m)
	if !ok {
		return false
	}
	return slices.Contains(ex.Inputs(), k) ||
		slices.ContainsFunc(ex.Request().Parameters(), func(p aot.Parameter) bool { return p.Name() == k })
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
