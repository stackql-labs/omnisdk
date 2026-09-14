package omnisdk

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/apply"
	"github.com/stackql-labs/omnisdk/internal/effect/dispatch"
	"github.com/stackql-labs/omnisdk/internal/effect/docrun"
	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/merge"
	"github.com/stackql-labs/omnisdk/internal/semantics"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/unwind"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/schemaxml"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

// ManagedResource is one key a run converges: what it is, what it should look like, and what it
// needs from its neighbours. Desired is opaque — nothing between here and the wire parses it.
type ManagedResource interface {
	// Key addresses the resource beneath the collection, e.g. "aws/ec2/vpc". Its leading segments
	// name the provider and service, which is how an effect is routed.
	Key() string
	// Provider is the registry provider serving it, e.g. "aws".
	Provider() string
	// Address is the resource's document address relative to the provider, e.g. "ec2.vpcs". The
	// operation run against it is chosen by verb and signature, so a resource names WHAT it is and
	// the document says which call that means.
	Address() string
	// Identity is the path to the minted id in the projected response, e.g. "line_items.VpcId".
	Identity() string
	// AddressedBy is the parameter carrying an existing object's identity, so a read or a delete can
	// be built from the ledger entry alone.
	AddressedBy() string
	// CorrelationParam is the parameter taking the correlation stamp, empty where the provider
	// offers no writable searchable field — in which case losing the ledger orphans the resource.
	CorrelationParam() string
	// Desired is the resolved intent to converge on.
	Desired() []byte
	// Params address the object on the wire.
	Params() map[string]string
	// Inbound is what arrives from sibling resources: each names a sibling whose recorded identity
	// lands in this resource's inbox. It is the dependency a β edge carries inside a query graph,
	// carried through the ledger instead because each effect is wrapped in its own durable writes.
	//
	// A sibling is named the same way as Key; the collection name is applied by Converge, which is
	// what lets a blueprint be written once and applied under any name.
	Inbound() []Arrival
	// Via reshapes the inbox into this resource's parameters — T_in, the same contract a query uses,
	// for the same reason: the shape one provider emits is frequently not the shape the next call
	// accepts. Empty is identity.
	Via() (dslType, program string)
}

// NewResource builds a ManagedResource. A client that assembles its own deployment — rather than
// naming a blueprint — uses this.
func NewResource(key, provider, address string, desired []byte, params map[string]string,
	inbound []Arrival, viaType, viaProgram, identity, addressedBy, correlationParam string) ManagedResource {
	return resource{key: key, provider: provider, address: address, desired: desired, params: params,
		inbound: inbound, viaType: viaType, viaProgram: viaProgram,
		identity: identity, addressedBy: addressedBy, correlation: correlationParam}
}

// Arrival is one value reaching a resource from a sibling in the same collection.
type Arrival struct {
	// From is the sibling's key, written as Key is.
	From string
	// As is the name it takes in the inbox; empty means From unchanged.
	As string
}

type resource struct {
	key                 string
	provider            string
	address             string
	desired             []byte
	params              map[string]string
	inbound             []Arrival
	viaType, viaProgram string
	identity            string
	addressedBy         string
	correlation         string
}

func (r resource) Key() string               { return r.key }
func (r resource) Provider() string          { return r.provider }
func (r resource) Address() string           { return r.address }
func (r resource) Identity() string          { return r.identity }
func (r resource) AddressedBy() string       { return r.addressedBy }
func (r resource) CorrelationParam() string  { return r.correlation }
func (r resource) Desired() []byte           { return r.desired }
func (r resource) Params() map[string]string { return r.params }
func (r resource) Inbound() []Arrival        { return r.inbound }
func (r resource) Via() (string, string)     { return r.viaType, r.viaProgram }

// Converge applies a set of resources as one collection, and is the whole IaC entry point: a client
// issues the imperative and the system works out idempotence, ordering, locking and compensation.
//
//   - registry is the provider-document root. Every effect is compiled from the document that
//     declares it, so a new service is a document plus its residue rather than Go code.
//   - name is the collection: the ledger key prefix, the lease scope, and the correlation tag
//     stamped on each object. It is the handle a later run uses to address the same resources.
//   - state holds the ledger and run journals. Local disk only — O_EXCL and link are unreliable on a
//     network share. One state directory holds many collections, separated by name.
//   - runID names this run's journal; empty means a UTC timestamp.
//   - resources are what to converge. Order is irrelevant: dependencies travel through bindings.
//   - args carries credentials, an endpoint override and tuning, exactly as a query does.
//
// Opening the returned Plan performs the run; the rows report what each key did.
func Converge(registry, name, state, runID string, resources []ManagedResource, args Args) (Plan, error) {
	switch {
	case name == "":
		return nil, fmt.Errorf("omnisdk: collection name is required")
	case state == "":
		return nil, fmt.Errorf("omnisdk: state directory is required")
	case len(resources) == 0:
		return nil, fmt.Errorf("omnisdk: no resources to converge")
	}
	effector, sem, exchanges, err := providerWiring(registry, resources, args)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		runID = time.Now().UTC().Format("20060102T150405Z")
	}
	return &convergePlan{name: name, state: state, runID: runID, resources: resources,
		effector: effector, semantics: sem, exchanges: exchanges, scope: args.Params}, nil
}

// providerWiring builds an effector per provider the resources name, and the semantics derived from
// their documents.
//
// Nothing here is a table of known services: a resource states its provider, its document address
// and the residue its document leaves unsaid, so a new service is data a caller supplies rather than
// an entry someone adds to this package.
func providerWiring(registry string, resources []ManagedResource, args Args) (facade.Effector, facade.Semantics, map[string]string, error) {
	reg, err := openRegistry(registry)
	if err != nil {
		return nil, nil, nil, err
	}
	dslReg, err := dsl.NewRegistry(append(gotemplate.Evaluators(), schemaxml.New())...)
	if err != nil {
		return nil, nil, nil, err
	}

	// A resource names a KEY; the effector resolves a published ADDRESS. Translating here keeps a
	// resource free of the registry's namespacing, so the same declaration survives a registry that
	// renames its providers.
	exchanges := map[string]string{}
	residues := map[string]map[string]docrun.Residue{}
	catalogs := map[string]aot.Catalog{}
	var decls []semantics.Declaration

	for _, r := range resources {
		if r.Provider() == "" || r.Address() == "" {
			return nil, nil, nil, fmt.Errorf("omnisdk: resource %q names no provider or address", r.Key())
		}
		cat, ok := catalogs[r.Provider()]
		if !ok {
			cat, err = reg.Catalog(r.Provider())
			if err != nil {
				return nil, nil, nil, fmt.Errorf("omnisdk: provider %q: %w", r.Provider(), err)
			}
			catalogs[r.Provider()] = cat
			residues[r.Provider()] = map[string]docrun.Residue{}
		}
		qualified, err := qualify(cat, r.Address())
		if err != nil {
			return nil, nil, nil, err
		}
		residues[r.Provider()][qualified] = docrun.NewResidue(r.Identity(), r.AddressedBy(), r.CorrelationParam())
		for _, verb := range []string{"insert", "update", "delete", "select"} {
			exchanges[exchangeOf(r.Key(), verb)] = exchangeOf(qualified, verb)
		}
		decls = append(decls, semantics.Declaration{
			Exchange: exchangeOf(qualified, "insert"),
			Form:     "insert",
			// The inverse of a create is the same resource's delete, chosen by signature when it
			// runs. Its absence in the document is what declares the create non-invertible.
			Inverse: exchangeOf(qualified, "delete"),
			// A document does not say whether a delete loses anything, so a derived inverse is lossy
			// until something states otherwise — which errs toward asking.
			Fidelity: facade.FidelityLossy,
		})
	}

	routes := map[string]facade.Effector{}
	for prov, cat := range catalogs {
		var sec aot.Security
		if pr := cat.Provider(); pr != nil {
			sec = pr.Security()
		}
		opts, err := docOptions(args, sec)
		if err != nil {
			return nil, nil, nil, err
		}
		if sec != nil {
			opts = append(opts, docx.WithProviderSecurity(sec))
		}
		routes[prov] = docrun.New(cat, dslReg, residues[prov], opts...)
	}

	sem, err := semantics.New(decls)
	if err != nil {
		return nil, nil, nil, err
	}
	return byProvider(resources, routes), sem, exchanges, nil
}

// byProvider routes each key to the provider that declared it, so a collection spanning providers
// reaches each one.
func byProvider(resources []ManagedResource, routes map[string]facade.Effector) facade.Effector {
	prefixes := map[string]facade.Effector{}
	for _, r := range resources {
		if eff, ok := routes[r.Provider()]; ok {
			prefixes[r.Key()] = eff
		}
	}
	return dispatch.New(prefixes)
}

// exchangeOf names an operation: an address and the verb to run against it. Which concrete call that
// means is the document's statement, chosen by signature when it runs.
func exchangeOf(addr, verb string) string { return addr + ":" + verb }

// qualify resolves a service-relative address to the one the catalog actually publishes.
//
// A provider's own name is not the prefix its addresses carry: document-derived providers are
// namespaced (stackql_unstable_aws) so they cannot collide with hand-authored catalog entries. The
// published list is the authority, and matching against it means a blueprint never states the
// namespace.
func qualify(cat aot.Catalog, addr string) (string, error) {
	suffix := "." + addr
	for _, p := range cat.Paths() {
		if strings.HasSuffix(p, suffix) {
			return p, nil
		}
	}
	return "", fmt.Errorf("omnisdk: the registry publishes no address ending %q", suffix)
}

// openRegistry resolves a registry root once per run.
func openRegistry(root string) (aot.Registry, error) {
	if root == "" {
		return nil, fmt.Errorf("omnisdk: a document registry is required")
	}
	return stackqldoc.OpenRegistry(os.DirFS(root))
}

type convergePlan struct {
	name      string
	state     string
	runID     string
	resources []ManagedResource
	effector  facade.Effector
	semantics facade.Semantics
	// exchanges translates a blueprint's resource-key exchange name into the registry address the
	// effector resolves.
	exchanges map[string]string
	// scope is what the run supplies to every step — a region, a project, a subscription.
	scope map[string]string
}

// leaseTTL bounds how long a dead run can hold the collection before another may break it.
const leaseTTL = 5 * time.Minute

func (p *convergePlan) Open(ctx context.Context) (Rows, error) {
	log, err := ledger.NewFile(filepath.Join(p.state, "ledger"))
	if err != nil {
		return nil, err
	}
	journals, err := journal.NewFiles(filepath.Join(p.state, "journal"))
	if err != nil {
		return nil, err
	}
	runner := apply.New(log, journals, lease.NewLeaser(log, time.Now), merge.ThreeWay(),
		p.effector, p.semantics, unwind.New(log, journals, p.semantics, p.effector), lease.All(), leaseTTL)

	dslReg, err := dsl.NewRegistry(append(gotemplate.Evaluators(), schemaxml.New())...)
	if err != nil {
		return nil, err
	}
	steps := make([]apply.Step, 0, len(p.resources))
	for _, r := range p.resources {
		inbound := make([]apply.Arrival, 0, len(r.Inbound()))
		for _, in := range r.Inbound() {
			name := in.As
			if name == "" {
				name = in.From
			}
			inbound = append(inbound, apply.Arrival{From: facade.LedgerKey(p.name + "/" + in.From), As: name})
		}
		exchange, ok := p.exchanges[exchangeOf(r.Key(), "insert")]
		if !ok {
			return nil, fmt.Errorf("omnisdk: no document operation for %q", r.Key())
		}
		// Scope travels with every step: a region or a project is stated once for the run, and the
		// document declares it as a server variable each call needs. A resource's own parameters win,
		// being the more specific statement.
		params := make(map[string]string, len(p.scope)+len(r.Params()))
		maps.Copy(params, p.scope)
		maps.Copy(params, r.Params())
		step := apply.Step{
			Key:      facade.LedgerKey(p.name + "/" + r.Key()),
			Exchange: exchange,
			Desired:  r.Desired(),
			Params:   params,
			Inbound:  inbound,
		}
		if typ, body := r.Via(); typ != "" {
			step.Via = docx.InboundProgram(dslReg, typ, body)
		}
		steps = append(steps, step)
	}

	res, err := runner.Apply(ctx, p.runID, p.name, steps)
	if err != nil {
		return nil, err
	}
	return convergeRows(ctx, log, res), nil
}

// convergeRows reports one row per key the run touched, and a final row when the run failed, so a
// partial compensation is visible rather than implied by an absence.
func convergeRows(ctx context.Context, log facade.Ledger, res apply.Result) Rows {
	// A key applied and then compensated must not read as "applied": the run left nothing behind,
	// and reporting otherwise sends someone looking for a resource that is gone.
	undone := map[facade.LedgerKey]bool{}
	if res.Unwound != nil {
		for _, k := range res.Unwound.Compensated {
			undone[k] = true
		}
	}
	var out []Row
	for _, k := range res.Applied {
		row := Row{"key": string(k), "status": "applied"}
		if undone[k] {
			row["status"] = "applied, then compensated"
		}
		if e, _, found, err := log.Get(ctx, k); err == nil && found {
			row["identity"] = string(e.Identity())
		}
		out = append(out, row)
	}
	if res.Err != nil {
		row := Row{"key": string(res.Failed), "status": "failed", "error": res.Err.Error()}
		if res.Unwound != nil {
			row["compensated"] = keyStrings(res.Unwound.Compensated)
			row["outstanding"] = keyStrings(res.Unwound.Outstanding)
			if !res.Unwound.Complete() {
				row["status"] = "failed; partially compensated"
			}
		}
		out = append(out, row)
	}
	return &sliceRows{rows: out, i: -1}
}

func keyStrings(keys []facade.LedgerKey) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = string(k)
	}
	return out
}

// sliceRows serves an already-materialised result through the same cursor a query uses, so a
// consumer needs no second shape.
type sliceRows struct {
	rows []Row
	i    int
}

func (s *sliceRows) Next() bool {
	s.i++
	return s.i < len(s.rows)
}

func (s *sliceRows) Row() Row   { return s.rows[s.i] }
func (s *sliceRows) Err() error { return nil }
func (s *sliceRows) Close() error {
	s.i = len(s.rows)
	return nil
}
