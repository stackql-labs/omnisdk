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
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/sdk"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/unwind"
)

// NetworkProvision is a two-step AWS network: a VPC, then a subnet inside it. The subnet's VpcId is
// not known until the VPC exists, so the dependency is implicit — inside a single query graph that
// is a β edge, and here it travels through the ledger, because each effect is wrapped in its own
// durable writes.
//
// Unlike the provision METHOD, which issues both creates and remembers nothing, this converges: a
// re-run against unchanged intent issues no calls, a run that fails part-way compensates what it
// created, and an object whose ledger entry is missing is rediscovered by its correlation tag
// rather than duplicated.
type NetworkProvision struct {
	// Region is the AWS region. Scope, never inferred.
	Region string
	// VPCCidr and SubnetCidr are the CIDR blocks to converge on. Both are immutable in EC2, so a
	// change to either is refused rather than silently re-created.
	VPCCidr    string
	SubnetCidr string
	// VPCTags and SubnetTags are stamped at create alongside the correlation key. They are not
	// converged in this version: the read does not extract the tag set, so a tag that drifts is
	// not corrected.
	VPCTags    map[string]string
	SubnetTags map[string]string
	// StateDir holds the ledger and the run journals. Local disk: O_EXCL and link are unreliable on
	// NFSv3, so a network share is not a supported backing store.
	StateDir string
	// Name is the collection: the ledger key prefix, the lease scope, and part of the correlation
	// tag stamped on the objects. Several collections share one StateDir.
	Name string
	// RunID names this run's journal. Defaults to a timestamp.
	RunID string
	// LeaseTTL bounds how long a dead run can hold the collection. Defaults to five minutes.
	LeaseTTL time.Duration
}

// awsNetworkDecls declares the per-provider semantics. Each create names its inverse, which makes
// the plan reversible ahead of time; neither declares an update, because an EC2 create mints a new
// object rather than converging an existing one.
var awsNetworkDecls = []semantics.Declaration{
	{Exchange: "CreateVpc", Form: "insert", Inverse: "DeleteVpc", Fidelity: facade.FidelityExact},
	{Exchange: "CreateSubnet", Form: "insert", Inverse: "DeleteSubnet", Fidelity: facade.FidelityExact},
}

// NewNetworkProvision plans the run. Opening it applies; the rows report what each step did.
func NewNetworkProvision(spec NetworkProvision, args Args) (Plan, error) {
	switch {
	case spec.Region == "":
		return nil, fmt.Errorf("omnisdk: region is required")
	case spec.VPCCidr == "" || spec.SubnetCidr == "":
		return nil, fmt.Errorf("omnisdk: vpc and subnet CIDR blocks are required")
	case spec.StateDir == "":
		return nil, fmt.Errorf("omnisdk: state directory is required")
	case spec.Name == "":
		return nil, fmt.Errorf("omnisdk: network name is required")
	}
	creds, err := awsCreds(args)
	if err != nil {
		return nil, err
	}
	if spec.RunID == "" {
		spec.RunID = time.Now().UTC().Format("20060102T150405Z")
	}
	if spec.LeaseTTL == 0 {
		spec.LeaseTTL = 5 * time.Minute
	}
	return &networkProvision{spec: spec, creds: creds, args: args}, nil
}

type networkProvision struct {
	spec  NetworkProvision
	creds sdk.Credentials
	args  Args
}

func (p *networkProvision) Open(ctx context.Context) (Rows, error) {
	log, err := ledger.NewFile(filepath.Join(p.spec.StateDir, "ledger"))
	if err != nil {
		return nil, err
	}
	journals, err := journal.NewFiles(filepath.Join(p.spec.StateDir, "journal"))
	if err != nil {
		return nil, err
	}
	sem, err := semantics.New(awsNetworkDecls)
	if err != nil {
		return nil, err
	}
	eff := awsec2.New(p.spec.Region, p.creds, p.args.Endpoint)
	runner := apply.New(log, journals, lease.NewLeaser(log, time.Now), merge.ThreeWay(), eff, sem,
		unwind.New(log, journals, sem, eff), lease.All(), p.spec.LeaseTTL)

	// The collection is the scope; the provider and service belong to each key's address, not above
	// it. A collection is a set of keys a run manages together and spans providers in general — a
	// DNS record at another provider joins this one as <name>/cloudflare/dns/record.
	scope := p.spec.Name
	vpcKey := facade.LedgerKey(scope + "/aws/ec2/vpc")
	subnetKey := facade.LedgerKey(scope + "/aws/ec2/subnet")

	res, err := runner.Apply(ctx, p.spec.RunID, scope, []apply.Step{
		{
			Key:      vpcKey,
			Exchange: "CreateVpc",
			Desired:  cidrDoc(p.spec.VPCCidr),
			Params:   tagParams(p.spec.VPCTags),
		},
		{
			Key:      subnetKey,
			Exchange: "CreateSubnet",
			Desired:  cidrDoc(p.spec.SubnetCidr),
			Params:   tagParams(p.spec.SubnetTags),
			// The subnet cannot be addressed until the VPC's id exists.
			Bindings: map[string]facade.LedgerKey{"VpcId": vpcKey},
		},
	})
	if err != nil {
		return nil, err
	}
	return provisionRows(ctx, log, res), nil
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

// provisionRows reports one row per key the run touched, and a final row when the run failed, so a
// partial compensation is visible rather than implied by an absence.
func provisionRows(ctx context.Context, log facade.Ledger, res apply.Result) Rows {
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
