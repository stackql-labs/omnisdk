package omnisdk

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/effect/dispatch"
	"github.com/stackql-labs/omnisdk/internal/effect/docrun"
	"github.com/stackql-labs/omnisdk/internal/semantics"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
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
	DebugCorrelationParam() DebugCorrelationParam
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
	inbound []Arrival, viaType, viaProgram, identity, addressedBy, debugCorrelationTarget string) ManagedResource {
	return resource{key: key, provider: provider, address: address, desired: desired, params: params,
		inbound: inbound, viaType: viaType, viaProgram: viaProgram,
		identity: identity, addressedBy: addressedBy, debugTarget: debugCorrelationTarget}
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
	debugTarget         string
}

func (r resource) Key() string         { return r.key }
func (r resource) Provider() string    { return r.provider }
func (r resource) Address() string     { return r.address }
func (r resource) Identity() string    { return r.identity }
func (r resource) AddressedBy() string { return r.addressedBy }
func (r resource) DebugCorrelationParam() DebugCorrelationParam {
	return DebugCorrelationParam{Target: r.debugTarget}
}
func (r resource) Desired() []byte           { return r.desired }
func (r resource) Params() map[string]string { return r.params }
func (r resource) Inbound() []Arrival        { return r.inbound }
func (r resource) Via() (string, string)     { return r.viaType, r.viaProgram }

// converge applies a set of resources as one collection: the desired-state form of a converging
// run, reached through Prepare with DesiredState and NewLedger. A client issues the imperative and the
// system works out idempotence, ordering, concurrency and compensation.
//
//   - registry is the provider-document root. Every effect is compiled from the document that
//     declares it, so a new service is a document plus its residue rather than Go code.
//   - name is the collection used by later runs to address the same resources.
//   - state holds the ledger and run journals. Local disk only — O_EXCL and link are unreliable on a
//     network share. One state directory holds many collections, separated by name.
//   - runID names this run's journal.
//   - resources are what to converge. Order is irrelevant: dependencies travel through bindings.
//   - args carries credentials, an endpoint override and tuning, exactly as a query does.
//
// Opening the returned Plan performs the run; the rows report what each key did.
func converge(registry, name, state, runID string, resources []ManagedResource, args Args) (Plan, error) {
	switch {
	case name == "":
		return nil, fmt.Errorf("omnisdk: collection name is required")
	case state == "":
		return nil, fmt.Errorf("omnisdk: state directory is required")
	case len(resources) == 0:
		return nil, fmt.Errorf("omnisdk: no resources to converge")
	}
	return newDesiredPlan(registry, name, state, runID, resources, args)
}

// providerWiring builds an effector per provider the resources name, and the semantics declared over
// them: what undoes a failed run through its journal.
//
// Nothing here is a table of known services: a resource states its provider, its document address
// and the residue its document leaves unsaid, so a new service is data a caller supplies rather than
// an entry someone adds to this package.
func providerWiring(registry string, resources []ManagedResource, args Args) (facade.Effector, facade.Semantics, error) {
	reg, err := openRegistry(registry, args.documentOptions()...)
	if err != nil {
		return nil, nil, err
	}
	dslReg, err := dsl.NewRegistry(append(gotemplate.Evaluators(), schemaxml.New())...)
	if err != nil {
		return nil, nil, err
	}

	// A resource names a KEY; the effector resolves a published ADDRESS. Translating here keeps a
	// resource free of the registry's namespacing, so the same declaration survives a registry that
	// renames its providers.
	residues := map[string]map[string]docrun.Residue{}
	catalogs := map[string]aot.Catalog{}
	var decls []semantics.Declaration

	for _, r := range resources {
		if r.Provider() == "" || r.Address() == "" {
			return nil, nil, fmt.Errorf("omnisdk: resource %q names no provider or address", r.Key())
		}
		cat, ok := catalogs[r.Provider()]
		if !ok {
			cat, err = reg.Catalog(r.Provider())
			if err != nil {
				return nil, nil, fmt.Errorf("omnisdk: provider %q: %w", r.Provider(), err)
			}
			catalogs[r.Provider()] = cat
			residues[r.Provider()] = map[string]docrun.Residue{}
		}
		qualified, err := qualify(cat, r.Address())
		if err != nil {
			return nil, nil, err
		}
		residues[r.Provider()][qualified] = docrun.NewResidue(r.Identity(), r.AddressedBy())
		// The inverse of a create is the same resource's delete, chosen by signature when it runs; a
		// resource made by a whole-object PUT is undone the same way. Its absence in the document is
		// what declares the create non-invertible. A document does not say whether a delete loses
		// anything, so a derived inverse is lossy until something states otherwise — which errs
		// toward asking.
		for _, verb := range []string{"insert", "replace"} {
			decls = append(decls, semantics.Declaration{
				Exchange: exchangeOf(qualified, verb),
				Form:     "insert",
				Inverse:  exchangeOf(qualified, "delete"),
				Fidelity: facade.FidelityLossy,
			})
		}
	}

	routes := map[string]facade.Effector{}
	for prov, cat := range catalogs {
		opts, err := providerOptions(args, cat.Provider())
		if err != nil {
			return nil, nil, err
		}
		routes[prov] = docrun.New(cat, analyzerFor(cat), principalOf(args), dslReg, residues[prov], opts...)
	}

	sem, err := semantics.New(decls)
	if err != nil {
		return nil, nil, err
	}
	return byProvider(resources, routes), sem, nil
}

// byProvider routes each key to the provider that declared it, so a collection spanning providers
// reaches each one.
func byProvider(resources []ManagedResource, routes map[string]facade.Effector) facade.Effector {
	effects := map[string]facade.Effector{}
	for _, r := range resources {
		if eff, ok := routes[r.Provider()]; ok {
			effects[r.Key()] = eff
		}
	}
	return dispatch.New(effects)
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
	// Only the named service's document is read; listing every address would parse them all.
	if service, resource, ok := strings.Cut(addr, "."); ok {
		if a, ok := cat.Address(service, resource); ok {
			return a, nil
		}
	}
	return "", fmt.Errorf("omnisdk: the registry publishes no address ending %q", "."+addr)
}

// openRegistry resolves a registry root once per run.
func openRegistry(root string, opts ...stackqldoc.Option) (aot.Registry, error) {
	if root == "" {
		return nil, fmt.Errorf("omnisdk: a document registry is required")
	}
	// Checked before opening: os.DirFS defers the error to the first read, which surfaces as
	// "open .: no such file or directory" and names nothing the caller can act on.
	if _, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("omnisdk: document registry %q: %w", root, err)
	}
	return stackqldoc.OpenRegistry(os.DirFS(root), append([]stackqldoc.Option{docCacheOption(root)}, opts...)...)
}

// leaseTTL bounds how long a dead run can hold the collection before another may break it.
const leaseTTL = 5 * time.Minute

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
