package omnisdk

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/apply"
	"github.com/stackql-labs/omnisdk/internal/effect/awsec2"
	"github.com/stackql-labs/omnisdk/internal/effect/dispatch"
	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/merge"
	"github.com/stackql-labs/omnisdk/internal/semantics"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/unwind"
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
	// Bindings resolve a parameter from another key's recorded identity — the dependency a β edge
	// carries inside a single query graph, carried through the ledger instead because each effect is
	// wrapped in its own durable writes. The key named is a sibling in the same collection, written
	// the same way as Key; the collection name is applied by Converge, which is what lets a
	// blueprint be written once and applied under any name.
	Bindings() map[string]string
}

// NewResource builds a ManagedResource. A client that assembles its own deployment — rather than
// naming a blueprint — uses this.
func NewResource(key, exchange string, desired []byte, params, bindings map[string]string) ManagedResource {
	return resource{key: key, exchange: exchange, desired: desired, params: params, bindings: bindings}
}

type resource struct {
	key      string
	exchange string
	desired  []byte
	params   map[string]string
	bindings map[string]string
}

func (r resource) Key() string                 { return r.key }
func (r resource) Exchange() string            { return r.exchange }
func (r resource) Desired() []byte             { return r.desired }
func (r resource) Params() map[string]string   { return r.params }
func (r resource) Bindings() map[string]string { return r.bindings }

// Converge applies a set of resources as one collection, and is the whole IaC entry point: a client
// issues the imperative and the system works out idempotence, ordering, locking and compensation.
//
//   - name is the collection: the ledger key prefix, the lease scope, and the correlation tag
//     stamped on each object. It is the handle a later run uses to address the same resources.
//   - state holds the ledger and run journals. Local disk only — O_EXCL and link are unreliable on a
//     network share. One state directory holds many collections, separated by name.
//   - runID names this run's journal; empty means a UTC timestamp.
//   - resources are what to converge. Order is irrelevant: dependencies travel through bindings.
//   - args carries credentials, an endpoint override and tuning, exactly as a query does.
//
// Opening the returned Plan performs the run; the rows report what each key did.
func Converge(name, state, runID string, resources []ManagedResource, args Args) (Plan, error) {
	switch {
	case name == "":
		return nil, fmt.Errorf("omnisdk: collection name is required")
	case state == "":
		return nil, fmt.Errorf("omnisdk: state directory is required")
	case len(resources) == 0:
		return nil, fmt.Errorf("omnisdk: no resources to converge")
	}
	effector, sem, err := wiring(resources, args)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		runID = time.Now().UTC().Format("20060102T150405Z")
	}
	return &convergePlan{name: name, state: state, runID: runID, resources: resources, effector: effector, semantics: sem}, nil
}

// providers maps a key-address prefix to the provider that services it. Adding a provider is an
// entry here; nothing above changes.
var providers = map[string]func(Args) (facade.Effector, []semantics.Declaration, error){
	"aws/ec2": func(args Args) (facade.Effector, []semantics.Declaration, error) {
		creds, err := awsCreds(args)
		if err != nil {
			return nil, nil, err
		}
		region := args.param("region")
		if region == "" {
			return nil, nil, fmt.Errorf("omnisdk: region is required for aws/ec2 resources")
		}
		return awsec2.New(region, creds, args.Endpoint), awsec2.Declarations(), nil
	},
}

// wiring resolves an effector per provider the resources touch, and routes between them. Only the
// providers actually addressed are constructed, so a deployment that never mentions one needs no
// credentials for it.
func wiring(resources []ManagedResource, args Args) (facade.Effector, facade.Semantics, error) {
	routes := map[string]facade.Effector{}
	var decls []semantics.Declaration
	for _, r := range resources {
		prefix, ok := providerFor(r.Key())
		if !ok {
			return nil, nil, fmt.Errorf("omnisdk: no provider for resource key %q", r.Key())
		}
		if _, built := routes[prefix]; built {
			continue
		}
		eff, d, err := providers[prefix](args)
		if err != nil {
			return nil, nil, err
		}
		routes[prefix], decls = eff, append(decls, d...)
	}
	sem, err := semantics.New(decls)
	if err != nil {
		return nil, nil, err
	}
	return dispatch.New(routes), sem, nil
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

	steps := make([]apply.Step, 0, len(p.resources))
	for _, r := range p.resources {
		bindings := make(map[string]facade.LedgerKey, len(r.Bindings()))
		for param, key := range r.Bindings() {
			bindings[param] = facade.LedgerKey(p.name + "/" + key)
		}
		steps = append(steps, apply.Step{
			Key:      facade.LedgerKey(p.name + "/" + r.Key()),
			Exchange: r.Exchange(),
			Desired:  r.Desired(),
			Params:   r.Params(),
			Bindings: bindings,
		})
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
