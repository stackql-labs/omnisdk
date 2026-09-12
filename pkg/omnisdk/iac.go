package omnisdk

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/stackql-labs/omnisdk/internal/apply"
	"github.com/stackql-labs/omnisdk/internal/effect/awsec2"
	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/merge"
	"github.com/stackql-labs/omnisdk/internal/semantics"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/unwind"
)

// ManagedResource is one key a deployment converges: what it is, what it should look like, and what
// it needs from its neighbours. Desired is opaque — nothing between here and the wire parses it.
type ManagedResource interface {
	// Key addresses the resource within the collection, e.g. "aws/ec2/vpc".
	Key() string
	// Exchange is the exchange that creates it.
	Exchange() string
	// Desired is the resolved intent to converge on.
	Desired() []byte
	// Params address the object on the wire.
	Params() map[string]string
	// Bindings resolve a parameter from another resource's recorded identity — the dependency that
	// a β edge carries inside a single query graph, carried through the ledger instead because each
	// effect is wrapped in its own durable writes.
	Bindings() map[string]string
}

// Deployment is a collection of resources to converge together. Implementations are provided by
// this package: the interface is sealed, so a caller composes a deployment from a constructor
// rather than supplying provider wiring of their own.
type Deployment interface {
	// Name is the collection: the ledger key prefix, the lease scope, and part of the correlation
	// tag stamped on each object.
	Name() string
	Resources() []ManagedResource
	// wiring supplies the provider seam. Unexported, which seals the interface.
	wiring(Args) (facade.Effector, facade.Semantics, error)
}

// resource is the ordinary implementation behind ManagedResource.
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

// awsNetwork is a VPC and a subnet inside it. The subnet's VpcId is not known until the VPC exists,
// so the dependency is implicit rather than declared.
type awsNetwork struct {
	name      string
	region    string
	resources []ManagedResource
}

// AWSNetwork describes a VPC and a subnet in it, as a collection named by name.
//
// Both CIDRs are immutable in EC2, so a change to either is refused rather than silently
// re-created. Tags are stamped at create alongside the correlation key and are not converged: the
// read does not extract the tag set, so a tag edited elsewhere is not corrected.
func AWSNetwork(name, region, vpcCidr, subnetCidr string, vpcTags, subnetTags map[string]string) (Deployment, error) {
	switch {
	case name == "":
		return nil, fmt.Errorf("omnisdk: collection name is required")
	case region == "":
		return nil, fmt.Errorf("omnisdk: region is required")
	case vpcCidr == "" || subnetCidr == "":
		return nil, fmt.Errorf("omnisdk: vpc and subnet CIDR blocks are required")
	}
	const vpc, subnet = "aws/ec2/vpc", "aws/ec2/subnet"
	return &awsNetwork{
		name:   name,
		region: region,
		resources: []ManagedResource{
			resource{
				key:      vpc,
				exchange: "CreateVpc",
				desired:  cidrDoc(vpcCidr),
				params:   tagParams(vpcTags),
			},
			resource{
				key:      subnet,
				exchange: "CreateSubnet",
				desired:  cidrDoc(subnetCidr),
				params:   tagParams(subnetTags),
				bindings: map[string]string{"VpcId": name + "/" + vpc},
			},
		},
	}, nil
}

// AWSSecuredNetwork is AWSNetwork plus a security group in the VPC: a second binding hop, where the
// group depends on the VPC exactly as the subnet does.
//
// groupName and groupDescription are both required by EC2 and neither is converged — a group's name
// is immutable, so a change is refused rather than replaced.
func AWSSecuredNetwork(name, region, vpcCidr, subnetCidr, groupName, groupDescription string, vpcTags, subnetTags, groupTags map[string]string) (Deployment, error) {
	base, err := AWSNetwork(name, region, vpcCidr, subnetCidr, vpcTags, subnetTags)
	if err != nil {
		return nil, err
	}
	if groupName == "" || groupDescription == "" {
		return nil, fmt.Errorf("omnisdk: security group name and description are required")
	}
	d := base.(*awsNetwork)
	d.resources = append(d.resources, resource{
		key:      "aws/ec2/security-group",
		exchange: "CreateSecurityGroup",
		desired:  fmt.Appendf(nil, `{"GroupName":%q,"GroupDescription":%q}`, groupName, groupDescription),
		params:   tagParams(groupTags),
		bindings: map[string]string{"VpcId": name + "/aws/ec2/vpc"},
	})
	return d, nil
}

func (d *awsNetwork) Name() string                 { return d.name }
func (d *awsNetwork) Resources() []ManagedResource { return d.resources }

// awsNetworkDecls declares the per-provider semantics. Each create names its inverse, which makes
// the plan reversible ahead of time; neither declares an update, because an EC2 create mints a new
// object rather than converging an existing one.
var awsNetworkDecls = []semantics.Declaration{
	{Exchange: "CreateVpc", Form: "insert", Inverse: "DeleteVpc", Fidelity: facade.FidelityExact},
	{Exchange: "CreateSubnet", Form: "insert", Inverse: "DeleteSubnet", Fidelity: facade.FidelityExact},
}

func (d *awsNetwork) wiring(args Args) (facade.Effector, facade.Semantics, error) {
	creds, err := awsCreds(args)
	if err != nil {
		return nil, nil, err
	}
	sem, err := semantics.New(awsNetworkDecls)
	if err != nil {
		return nil, nil, err
	}
	return awsec2.New(d.region, creds, args.Endpoint), sem, nil
}

// Converge plans a run over a deployment. Opening it applies; the rows report what each step did.
//
// state is where the ledger and run journals live — local disk only, since O_EXCL and link are
// unreliable on a network share. One state directory holds many collections, separated by name.
// runID names this run's journal and defaults to a UTC timestamp.
func Converge(d Deployment, state, runID string, args Args) (Plan, error) {
	if d == nil {
		return nil, fmt.Errorf("omnisdk: deployment is required")
	}
	if state == "" {
		return nil, fmt.Errorf("omnisdk: state directory is required")
	}
	effector, sem, err := d.wiring(args)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		runID = time.Now().UTC().Format("20060102T150405Z")
	}
	return &convergePlan{dep: d, state: state, runID: runID, effector: effector, semantics: sem}, nil
}

type convergePlan struct {
	dep       Deployment
	state     string
	runID     string
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

	scope := p.dep.Name()
	steps := make([]apply.Step, 0, len(p.dep.Resources()))
	for _, r := range p.dep.Resources() {
		bindings := make(map[string]facade.LedgerKey, len(r.Bindings()))
		for param, key := range r.Bindings() {
			bindings[param] = facade.LedgerKey(key)
		}
		steps = append(steps, apply.Step{
			Key:      facade.LedgerKey(scope + "/" + r.Key()),
			Exchange: r.Exchange(),
			Desired:  r.Desired(),
			Params:   r.Params(),
			Bindings: bindings,
		})
	}

	res, err := runner.Apply(ctx, p.runID, scope, steps)
	if err != nil {
		return nil, err
	}
	return convergeRows(ctx, log, res), nil
}

func cidrDoc(cidr string) []byte { return fmt.Appendf(nil, `{"CidrBlock":%q}`, cidr) }

// tagParams marks each tag so the effector stamps it rather than sending it as a form parameter.
func tagParams(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		out[awsec2.TagParamPrefix+k] = v
	}
	return out
}

// convergeRows reports one row per key the run touched, and a final row when the run failed, so a
// partial compensation is visible rather than implied by an absence.
func convergeRows(ctx context.Context, log facade.Ledger, res apply.Result) Rows {
	// A key that was applied and then compensated must not read as "applied": the run left nothing
	// behind, and reporting otherwise would send someone looking for a resource that is gone.
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
