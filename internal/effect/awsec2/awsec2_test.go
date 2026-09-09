package awsec2_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
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

// ec2 is a stand-in for the EC2 Query API: it speaks the real wire shape — signed form POSTs in,
// XML out — and enforces the one piece of referential integrity that matters here, refusing to
// delete a VPC while a subnet remains in it.
type ec2 struct {
	mu      sync.Mutex
	vpcs    map[string]string // id → cidr
	subnets map[string]string // id → vpc id
	tags    map[string]string // id → correlation key
	actions []string
	failOn  string
	nextID  int
}

// byTag resolves the correlation filter EC2 exposes, which is what lets an object be rediscovered
// without the ledger.
func (e *ec2) byTag(r *http.Request, ids map[string]string) (string, bool) {
	if r.PostForm.Get("Filter.1.Name") != "tag:"+awsec2.CorrelationTag {
		return "", false
	}
	want := r.PostForm.Get("Filter.1.Value.1")
	for id := range ids {
		if e.tags[id] == want {
			return id, true
		}
	}
	return "", true
}

func newEC2() *ec2 {
	return &ec2{vpcs: map[string]string{}, subnets: map[string]string{}, tags: map[string]string{}}
}

func (e *ec2) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		act := r.PostForm.Get("Action")
		e.actions = append(e.actions, act)

		if act == e.failOn {
			// The Query API reports failure in the body with a 400.
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `<Response><Errors><Error><Code>InvalidParameterValue</Code></Error></Errors></Response>`)
			return
		}

		w.Header().Set("Content-Type", "text/xml")
		switch act {
		case "CreateVpc":
			e.nextID++
			id := fmt.Sprintf("vpc-%03d", e.nextID)
			e.vpcs[id] = r.PostForm.Get("CidrBlock")
			e.tags[id] = r.PostForm.Get("TagSpecification.1.Tag.1.Value")
			fmt.Fprintf(w, `<CreateVpcResponse><vpc><vpcId>%s</vpcId><cidrBlock>%s</cidrBlock></vpc></CreateVpcResponse>`, id, e.vpcs[id])
		case "CreateSubnet":
			vpc := r.PostForm.Get("VpcId")
			if _, ok := e.vpcs[vpc]; !ok {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `<Response><Errors><Error><Code>InvalidVpcID.NotFound</Code></Error></Errors></Response>`)
				return
			}
			e.nextID++
			id := fmt.Sprintf("subnet-%03d", e.nextID)
			e.subnets[id] = vpc
			e.tags[id] = r.PostForm.Get("TagSpecification.1.Tag.1.Value")
			fmt.Fprintf(w, `<CreateSubnetResponse><subnet><subnetId>%s</subnetId><cidrBlock>%s</cidrBlock></subnet></CreateSubnetResponse>`, id, r.PostForm.Get("CidrBlock"))
		case "DeleteVpc":
			id := r.PostForm.Get("VpcId")
			for _, vpc := range e.subnets {
				if vpc == id {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `<Response><Errors><Error><Code>DependencyViolation</Code></Error></Errors></Response>`)
					return
				}
			}
			delete(e.vpcs, id)
			fmt.Fprint(w, `<DeleteVpcResponse><return>true</return></DeleteVpcResponse>`)
		case "DeleteSubnet":
			delete(e.subnets, r.PostForm.Get("SubnetId"))
			fmt.Fprint(w, `<DeleteSubnetResponse><return>true</return></DeleteSubnetResponse>`)
		case "DescribeVpcs":
			id := r.PostForm.Get("VpcId.1")
			if found, filtered := e.byTag(r, e.vpcs); filtered {
				id = found
			}
			cidr, ok := e.vpcs[id]
			if !ok {
				fmt.Fprint(w, `<DescribeVpcsResponse><vpcSet></vpcSet></DescribeVpcsResponse>`)
				return
			}
			fmt.Fprintf(w, `<DescribeVpcsResponse><vpcSet><item><vpcId>%s</vpcId><cidrBlock>%s</cidrBlock></item></vpcSet></DescribeVpcsResponse>`, id, cidr)
		case "DescribeSubnets":
			id := r.PostForm.Get("SubnetId.1")
			if found, filtered := e.byTag(r, e.subnets); filtered {
				id = found
			}
			if _, ok := e.subnets[id]; !ok {
				fmt.Fprint(w, `<DescribeSubnetsResponse><subnetSet></subnetSet></DescribeSubnetsResponse>`)
				return
			}
			fmt.Fprintf(w, `<DescribeSubnetsResponse><subnetSet><item><subnetId>%s</subnetId><cidrBlock>10.0.1.0/24</cidrBlock></item></subnetSet></DescribeSubnetsResponse>`, id)
		default:
			http.Error(w, "unexpected action "+act, http.StatusBadRequest)
		}
	})
}

func (e *ec2) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.actions)
}

func (e *ec2) live() (vpcs, subnets int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.vpcs), len(e.subnets)
}

// decls are the per-provider semantics: each create names its inverse, which is what makes the
// plan reversible ahead of time.
var decls = []semantics.Declaration{
	{Exchange: "CreateVpc", Form: "insert", Inverse: "DeleteVpc", Fidelity: facade.FidelityExact},
	{Exchange: "CreateSubnet", Form: "insert", Inverse: "DeleteSubnet", Fidelity: facade.FidelityExact},
}

func fixture(t *testing.T, srv *httptest.Server) (apply.Runner, facade.Ledger) {
	t.Helper()
	log, err := ledger.NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	js, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatalf("journals: %v", err)
	}
	sem, err := semantics.New(decls)
	if err != nil {
		t.Fatalf("semantics: %v", err)
	}
	eff := awsec2.New("us-east-1", sdk.Credentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"}, srv.URL+"/")
	return apply.New(log, js, lease.NewLeaser(log, time.Now), merge.ThreeWay(), eff, sem, unwind.New(log, js, sem, eff), lease.All(), time.Minute), log
}

// The two steps of the premise: a VPC, then a subnet that cannot be addressed until the VPC's id
// exists. Inside one plan that is a β edge; here the same dependency travels through the ledger,
// because each effect is wrapped in its own durable writes.
func steps() []apply.Step {
	return []apply.Step{
		{
			Key:      "aws/ec2/vpc/demo",
			Exchange: "CreateVpc",
			Desired:  []byte(`{"CidrBlock":"10.0.0.0/16"}`),
		},
		{
			Key:      "aws/ec2/subnet/demo",
			Exchange: "CreateSubnet",
			Desired:  []byte(`{"CidrBlock":"10.0.1.0/24"}`),
			Bindings: map[string]facade.LedgerKey{"VpcId": "aws/ec2/vpc/demo"},
		},
	}
}

func TestTwoStepProvisionBindsSubnetToTheVpc(t *testing.T) {
	ctx := context.Background()
	api := newEC2()
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	r, log := fixture(t, srv)

	res, err := r.Apply(ctx, "run-1", "aws/ec2", steps())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Complete() {
		t.Fatalf("result = %+v, want both steps landed", res)
	}

	vpcs, subnets := api.live()
	if vpcs != 1 || subnets != 1 {
		t.Errorf("target holds %d vpcs and %d subnets, want 1 and 1", vpcs, subnets)
	}

	e, _, found, err := log.Get(ctx, "aws/ec2/vpc/demo")
	if err != nil || !found {
		t.Fatalf("vpc entry: found=%v err=%v", found, err)
	}
	if e.Phase() != facade.LedgerLive || string(e.Identity()) != "vpc-001" {
		t.Errorf("vpc entry = phase %v identity %s, want live vpc-001", e.Phase(), e.Identity())
	}

	sub, _, found, err := log.Get(ctx, "aws/ec2/subnet/demo")
	if err != nil || !found {
		t.Fatalf("subnet entry: found=%v err=%v", found, err)
	}
	if string(sub.Identity()) != "subnet-002" {
		t.Errorf("subnet identity = %s, want subnet-002", sub.Identity())
	}

	// The subnet create carried the VPC's minted id, which is the whole point of the binding.
	if got := api.seen(); !slices.Contains(got, "CreateSubnet") {
		t.Errorf("actions = %v, want a CreateSubnet", got)
	}
}

// The failing step is the dependent one, so the VPC has already landed and must be compensated.
// EC2 refuses to delete a VPC while a subnet remains, which is exactly the referential integrity
// the unwind leans on instead of modelling containment.
func TestSubnetFailureUnwindsTheVpc(t *testing.T) {
	ctx := context.Background()
	api := newEC2()
	api.failOn = "CreateSubnet"
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	r, log := fixture(t, srv)

	res, err := r.Apply(ctx, "run-1", "aws/ec2", steps())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Complete() {
		t.Fatal("result reports complete, want the subnet failure reported")
	}
	if res.Failed != "aws/ec2/subnet/demo" {
		t.Errorf("failed = %s, want the subnet", res.Failed)
	}
	if res.Unwound == nil || !res.Unwound.Complete() {
		t.Fatalf("unwound = %+v, want a complete compensation", res.Unwound)
	}

	if vpcs, subnets := api.live(); vpcs != 0 || subnets != 0 {
		t.Errorf("target holds %d vpcs and %d subnets, want nothing left behind", vpcs, subnets)
	}
	if !slices.Contains(api.seen(), "DeleteVpc") {
		t.Errorf("actions = %v, want the vpc compensated", api.seen())
	}
	if _, _, found, _ := log.Get(ctx, "aws/ec2/vpc/demo"); found {
		t.Error("ledger still holds the vpc after compensation")
	}
}

// A binding onto a key that never became live is an error, not an empty parameter: sending an
// unbound VpcId would create an orphan the ledger could not describe.
func TestBindingOnAMissingKeyIsRefused(t *testing.T) {
	ctx := context.Background()
	api := newEC2()
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	r, _ := fixture(t, srv)

	_, err := r.Apply(ctx, "run-1", "aws/ec2", []apply.Step{steps()[1]})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if slices.Contains(api.seen(), "CreateSubnet") {
		t.Errorf("actions = %v, want no call attempted with an unbound VpcId", api.seen())
	}
}

// Re-running the same intent must not create a second VPC: the log gives idempotence, and the live
// read gives convergence on top of it.
func TestReapplyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	api := newEC2()
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	r, _ := fixture(t, srv)

	if _, err := r.Apply(ctx, "run-1", "aws/ec2", steps()); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if _, err := r.Apply(ctx, "run-2", "aws/ec2", steps()); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	if vpcs, _ := api.live(); vpcs != 1 {
		t.Errorf("target holds %d vpcs after re-apply, want 1", vpcs)
	}
	// The second run converges rather than replaying: actual already satisfies the mutation, so
	// no create is issued at all.
	if n := strings.Count(strings.Join(api.seen(), " "), "CreateVpc"); n != 1 {
		t.Errorf("CreateVpc issued %d times, want 1 — the re-apply should converge, not replay", n)
	}
}

// A VPC's CIDR is immutable and EC2's create mints rather than upserts, so drift on a live entry
// must be refused rather than silently re-created. Declaring no update path is what says so.
func TestDriftOnAnImmutableResourceIsRefused(t *testing.T) {
	ctx := context.Background()
	api := newEC2()
	srv := httptest.NewServer(api.handler())
	defer srv.Close()
	r, _ := fixture(t, srv)

	if _, err := r.Apply(ctx, "run-1", "aws/ec2", steps()[:1]); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	changed := steps()[:1]
	changed[0].Desired = []byte(`{"CidrBlock":"172.16.0.0/16"}`)
	res, err := r.Apply(ctx, "run-2", "aws/ec2", changed)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res.Complete() {
		t.Fatal("changing an immutable CIDR reported success, want refusal")
	}
	if !strings.Contains(res.Err.Error(), "no update path") {
		t.Errorf("err = %v, want a refusal naming the missing update path", res.Err)
	}
	if vpcs, _ := api.live(); vpcs != 1 {
		t.Errorf("target holds %d vpcs, want the original untouched", vpcs)
	}
}

// The store is a cache, not the sole link to reality. With the ledger thrown away entirely, a
// re-apply must find what it already created by its correlation stamp and adopt it — not create a
// second copy. This is the property Terraform lacks, and why its state file feels so precious.
func TestLostLedgerRediscoversRatherThanDuplicates(t *testing.T) {
	ctx := context.Background()
	api := newEC2()
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	first, _ := fixture(t, srv)
	if _, err := first.Apply(ctx, "run-1", "aws/ec2", steps()); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if vpcs, subnets := api.live(); vpcs != 1 || subnets != 1 {
		t.Fatalf("after first apply: %d vpcs, %d subnets", vpcs, subnets)
	}

	// A completely fresh ledger: nothing recorded, as if the store had been lost.
	second, log := fixture(t, srv)
	res, err := second.Apply(ctx, "run-2", "aws/ec2", steps())
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if !res.Complete() {
		t.Fatalf("result = %+v, want the existing objects adopted", res)
	}

	if vpcs, subnets := api.live(); vpcs != 1 || subnets != 1 {
		t.Errorf("target holds %d vpcs and %d subnets, want the originals adopted rather than duplicated", vpcs, subnets)
	}
	e, _, found, err := log.Get(ctx, "aws/ec2/vpc/demo")
	if err != nil || !found {
		t.Fatalf("rebuilt entry: found=%v err=%v", found, err)
	}
	if e.Phase() != facade.LedgerLive || string(e.Identity()) != "vpc-001" {
		t.Errorf("rebuilt entry = phase %v identity %s, want live vpc-001", e.Phase(), e.Identity())
	}
}

// A read is addressed, never mutated. The subnet's bound VpcId is a CreateSubnet parameter, and
// sending it to DescribeSubnets is rejected outright by EC2 — so a read must not inherit the
// caller's parameters.
func TestReadDoesNotInheritCallerParameters(t *testing.T) {
	ctx := context.Background()
	var describeParams []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("Action") == "DescribeSubnets" {
			for k := range r.PostForm {
				describeParams = append(describeParams, k)
			}
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<DescribeSubnetsResponse><subnetSet></subnetSet></DescribeSubnetsResponse>`)
	}))
	defer srv.Close()

	eff := awsec2.New("us-east-1", sdk.Credentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"}, srv.URL+"/")
	if _, _, _, err := eff.Read(ctx, "CreateSubnet", "aws/ec2/demo/subnet", facade.EffectInput{
		Params: map[string]string{"VpcId": "vpc-001", "CidrBlock": "10.0.1.0/24"},
	}); err != nil {
		t.Fatalf("read: %v", err)
	}

	for _, p := range describeParams {
		if p == "VpcId" || p == "CidrBlock" {
			t.Errorf("DescribeSubnets received %q; a read takes only its address or filter", p)
		}
	}
	if !slices.Contains(describeParams, "Filter.1.Name") {
		t.Errorf("params = %v, want the correlation filter when identity is unknown", describeParams)
	}
}
