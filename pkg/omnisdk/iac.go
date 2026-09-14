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
	// Exchange creates it.
	Exchange() string
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
func NewResource(key, exchange string, desired []byte, params map[string]string, inbound []Arrival, viaType, viaProgram string) ManagedResource {
	return resource{key: key, exchange: exchange, desired: desired, params: params,
		inbound: inbound, viaType: viaType, viaProgram: viaProgram}
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
	exchange            string
	desired             []byte
	params              map[string]string
	inbound             []Arrival
	viaType, viaProgram string
}

func (r resource) Key() string               { return r.key }
func (r resource) Exchange() string          { return r.exchange }
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

// providers maps a key-address prefix to the provider that services it. Adding a provider is an
// entry here; nothing above changes.
// providers maps a key-address prefix to the provider that services it. Adding a provider is an
// entry here; nothing above changes.
//
// Every entry is document-driven: the effector compiles the operation the document declares, so a
// new service is a document plus its residue, not Go code. The prefix is what a resource key begins
// with, and the address is what the registry knows the same resource as.
var providers = map[string]provider{
	"aws/ec2": {
		registryProvider: "aws",
		// What a document does not state, per resource: where the identity is in the response, which
		// parameter addresses an existing object, and which parameter takes the correlation stamp.
		residue: map[string]docrun.Residue{
			// The identity path is against the PROJECTED response: the document's schema-driven
			// transform normalises every reply into line_items, so a create's minted id is a field
			// of the row rather than a path through the provider's own envelope.
			"aws/ec2/vpc": docrun.NewResidue(
				"line_items.VpcId", "VpcId", "TagSpecification.1.Tag.1.Value"),
			"aws/ec2/subnet": docrun.NewResidue(
				"line_items.SubnetId", "SubnetId", "TagSpecification.1.Tag.1.Value"),
		},
		// The registry address each resource key names.
		addresses: map[string]string{
			"aws/ec2/vpc":    "ec2.vpcs",
			"aws/ec2/subnet": "ec2.subnets",
		},
	},
}

// provider is one registry-backed provider and the residue its documents leave unstated.
type provider struct {
	registryProvider string
	residue          map[string]docrun.Residue
	addresses        map[string]string
}

// providerWiring resolves an effector per provider the resources touch, and routes between them. Only the
// providers actually addressed are constructed, so a deployment that never mentions one needs no
// credentials for it.
func providerWiring(registry string, resources []ManagedResource, args Args) (facade.Effector, facade.Semantics, map[string]string, error) {
	reg, err := openRegistry(registry)
	if err != nil {
		return nil, nil, nil, err
	}
	dslReg, err := dsl.NewRegistry(append(gotemplate.Evaluators(), schemaxml.New())...)
	if err != nil {
		return nil, nil, nil, err
	}

	// A blueprint names a resource KEY; the effector resolves a registry ADDRESS. Translating here
	// is what keeps a blueprint free of the provider's own naming, so the same blueprint survives a
	// document that renames its resources.
	exchanges := map[string]string{}
	routes := map[string]facade.Effector{}
	var decls []semantics.Declaration
	for _, r := range resources {
		prefix, ok := providerFor(r.Key())
		if !ok {
			return nil, nil, nil, fmt.Errorf("omnisdk: no provider for resource key %q", r.Key())
		}
		if _, built := routes[prefix]; built {
			continue
		}
		p := providers[prefix]
		cat, err := reg.Catalog(p.registryProvider)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("omnisdk: provider %q: %w", p.registryProvider, err)
		}
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
		// Residue is keyed by the registry address, which is what the effector resolves against.
		byAddress := map[string]docrun.Residue{}
		for key, res := range p.residue {
			qualified, err := qualify(cat, p.addresses[key])
			if err != nil {
				return nil, nil, nil, err
			}
			byAddress[qualified] = res
		}
		routes[prefix] = docrun.New(cat, dslReg, byAddress, opts...)

		// Semantics come from the documents: the inverse of a create is the same resource's delete,
		// and its absence is what declares the create non-invertible. No hand-authored table.
		for key, addr := range p.addresses {
			qualified, err := qualify(cat, addr)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, verb := range []string{"insert", "update", "delete", "select"} {
				exchanges[exchangeOf(key, verb)] = exchangeOf(qualified, verb)
			}
			decls = append(decls, semantics.Declaration{
				Exchange: exchangeOf(qualified, "insert"),
				Form:     "insert",
				Inverse:  exchangeOf(qualified, "delete"),
				// A document does not say whether a delete loses anything, so a derived inverse is
				// lossy until something states otherwise — which errs toward asking.
				Fidelity: facade.FidelityLossy,
			})
		}
	}
	sem, err := semantics.New(decls)
	if err != nil {
		return nil, nil, nil, err
	}
	return dispatch.New(routes), sem, exchanges, nil
}

// insertOf names the create for a resource key: the registry address and the verb, which is how a
// document-driven effector addresses an operation. A blueprint names WHAT to converge; which
// operation that is remains the document's statement.
func insertOf(key string) string { return exchangeOf(key, "insert") }

// deleteOf names the compensating operation for a resource key.
func deleteOf(key string) string { return exchangeOf(key, "delete") }

// exchangeOf builds an exchange name from a resource key and a verb. The provider prefix is applied
// at wiring time, where the catalog is known, so a blueprint stays free of it.
func exchangeOf(key, verb string) string { return key + ":" + verb }

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

// providerFor finds the longest registered prefix matching a resource key.
func providerFor(key string) (string, bool) {
	best := ""
	for prefix := range providers {
		if strings.HasPrefix(key, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	return best, best != ""
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
		exchange, ok := p.exchanges[r.Exchange()]
		if !ok {
			return nil, fmt.Errorf("omnisdk: no document operation for %q", r.Exchange())
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
