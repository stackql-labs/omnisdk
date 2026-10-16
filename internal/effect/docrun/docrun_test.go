package docrun_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/analyze"
	"github.com/stackql-labs/omnisdk/internal/docsem"
	"github.com/stackql-labs/omnisdk/internal/effect/docrun"
	"github.com/stackql-labs/omnisdk/internal/system_g/awsv4"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

// fixture builds the effector the way a caller would: one provider catalog, the document's own
// operations, and the residue the document does not state.
func fixture(t *testing.T, srv *httptest.Server) (docrun.Effector, string) {
	t.Helper()
	return fixtureAs(t, srv, facade.AnonymousPrincipal, analyze.PermitAll())
}

// fixtureAs is fixture acting on behalf of principal, under policy.
func fixtureAs(t *testing.T, srv *httptest.Server, principal string, policy facade.Policy) (docrun.Effector, string) {
	t.Helper()
	reg, err := stackqldoc.OpenRegistry(os.DirFS("../../../pkg/docparse/stackqldoc/testdata"))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	cat, err := reg.Catalog("aws")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	var addr string
	for _, p := range cat.Paths() {
		if strings.HasSuffix(p, ".ec2.vpcs") {
			addr = p
		}
	}
	if addr == "" {
		t.Fatal("ec2.vpcs not addressable")
	}
	dslReg, err := dsl.NewRegistry(gotemplate.Evaluators()...)
	if err != nil {
		t.Fatalf("dsl: %v", err)
	}
	eff := docrun.New(cat, analyze.New(docsem.Capabilities(cat), policy), principal, dslReg,
		map[string]docrun.Residue{
			addr: docrun.NewResidue("CreateVpcResponse.vpc.vpcId", "VpcId", ""),
		},
		docx.WithBaseURL(srv.URL),
		docx.WithAWSCredentials(awsv4.Credentials{AccessKeyID: "AKIATEST", SecretAccessKey: "secret"}),
	)
	return eff, addr
}

// A create runs entirely from the document: the operation is selected by verb and signature, the
// request is compiled from what the document declares, and the identity comes back off the response.
// No provider-specific code took part.
func TestCreateFromDocument(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("Action") + "|" + r.URL.Query().Get("CidrBlock")
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<CreateVpcResponse><vpc><vpcId>vpc-777</vpcId></vpc></CreateVpcResponse>`)
	}))
	defer srv.Close()

	eff, addr := fixture(t, srv)
	id, err := eff.Effect(context.Background(), addr+":insert", "demo/aws/ec2/vpc", facade.EffectInput{
		Mutation: []byte(`{"CidrBlock":"10.0.0.0/16"}`),
		// region is a server variable the document declares; it is scope, so the caller supplies it.
		Params: map[string]string{"region": "us-east-1"},
	})
	if err != nil {
		t.Fatalf("effect: %v", err)
	}
	if string(id) != "vpc-777" {
		t.Errorf("identity = %s, want vpc-777", id)
	}
	if !strings.HasPrefix(seen, "CreateVpc|10.0.0.0/16") {
		t.Errorf("wire = %q, want CreateVpc carrying the CIDR", seen)
	}
}

// The delete is the same path with a different verb, addressed by the recorded identity.
func TestDeleteFromDocument(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("Action") + "|" + r.URL.Query().Get("VpcId")
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<DeleteVpcResponse><return>true</return></DeleteVpcResponse>`)
	}))
	defer srv.Close()

	eff, addr := fixture(t, srv)
	if _, err := eff.Effect(context.Background(), addr+":delete", "demo/aws/ec2/vpc", facade.EffectInput{
		Identity: []byte("vpc-777"),
		Params:   map[string]string{"region": "us-east-1"},
	}); err != nil {
		t.Fatalf("effect: %v", err)
	}
	if seen != "DeleteVpc|vpc-777" {
		t.Errorf("wire = %q, want DeleteVpc addressed by the identity", seen)
	}
}

// A response is one value: what the call reported about itself beside what it returned. Metadata is
// elided from a flow's rows, never discarded — so a read hands back the body AND the status.
func TestReadCarriesMetadataAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-777</vpcId><cidrBlock>10.0.0.0/16</cidrBlock></item></vpcSet></DescribeVpcsResponse>`)
	}))
	defer srv.Close()

	eff, addr := fixture(t, srv)
	actual, identity, readable, err := eff.Read(context.Background(), addr+":select", "demo/aws/ec2/vpc", facade.EffectInput{
		Identity: []byte("vpc-777"),
		Params:   map[string]string{"region": "us-east-1"},
	})
	if err != nil || !readable {
		t.Fatalf("read: readable=%v err=%v", readable, err)
	}
	if !strings.Contains(string(actual), `"_response":{"status":"200"}`) {
		t.Errorf("actual = %s, want the response metadata reachable", actual)
	}
	if !strings.Contains(string(actual), "10.0.0.0/16") {
		t.Errorf("actual = %s, want the body too", actual)
	}
	// What we sent must not come back as what the target holds: convergence would then compare the
	// intent against itself and agree always.
	if strings.Contains(string(actual), `"region"`) {
		t.Errorf("actual = %s, want the echoed inputs stripped", actual)
	}
	if string(identity) != "vpc-777" {
		t.Errorf("identity = %s, want vpc-777", identity)
	}
}

// Drift goes to the update the document states for the field that changed. The VPC declares five
// operations under update; only ModifyVpcTenancy accepts InstanceTenancy, and an operation without a
// body is sent only what changed, so the unchanged CIDR does not rule it out.
func TestDriftRoutesToTheOperationAcceptingTheChange(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		seen = q.Get("Action") + "|" + q.Get("VpcId") + "|" + q.Get("InstanceTenancy") + "|" + q.Get("CidrBlock")
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<ModifyVpcTenancyResponse><return>true</return></ModifyVpcTenancyResponse>`)
	}))
	defer srv.Close()

	eff, addr := fixture(t, srv)
	in := facade.EffectInput{
		Mutation: []byte(`{"CidrBlock":"10.0.0.0/16","InstanceTenancy":"default"}`),
		Identity: []byte("vpc-1"),
		Params:   map[string]string{"region": "us-east-1"},
	}
	actual := []byte(`{"CidrBlock":"10.0.0.0/16","InstanceTenancy":"dedicated"}`)
	ex, sent, err := eff.Route(context.Background(), addr+":insert", "update", "demo/aws/ec2/vpc", in, actual)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if ex != addr+":update" || string(sent) != `{"InstanceTenancy":"default"}` {
		t.Fatalf("route = %s %s, want %s:update with only InstanceTenancy", ex, sent, addr)
	}
	in.Mutation = sent
	if _, err := eff.Effect(context.Background(), ex, "demo/aws/ec2/vpc", in); err != nil {
		t.Fatalf("effect: %v", err)
	}
	if seen != "ModifyVpcTenancy|vpc-1|default|" {
		t.Errorf("wire = %q, want ModifyVpcTenancy for vpc-1 carrying only the tenancy", seen)
	}
}

// A change no update operation accepts is refused, and so is removing a field through an operation
// that has no body to express the removal in.
func TestDriftNothingConvergesIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a refused drift reached the wire: %s", r.URL.RawQuery)
	}))
	defer srv.Close()
	eff, addr := fixture(t, srv)
	for name, c := range map[string]struct{ desired, actual string }{
		"unaccepted field": {`{"EnableDnsSupport":"true"}`, `{"EnableDnsSupport":"false"}`},
		"unset field":      {`{"InstanceTenancy":null}`, `{"InstanceTenancy":"dedicated"}`},
	} {
		in := facade.EffectInput{Mutation: []byte(c.desired), Identity: []byte("vpc-1"), Params: map[string]string{"region": "us-east-1"}}
		if ex, _, err := eff.Route(context.Background(), addr+":insert", "update", "demo/aws/ec2/vpc", in, []byte(c.actual)); err == nil {
			t.Errorf("%s: routed to %s; want a refusal", name, ex)
		}
	}
}

type onlyFor string

func (p onlyFor) Permit(_ context.Context, s facade.Statement, _ facade.Method) error {
	if s.Principal() != string(p) {
		return fmt.Errorf("%s may not act", s.Principal())
	}
	return nil
}

// An effect is analyzed on behalf of the effector's principal, so a policy deciding for someone else
// stops it before anything is sent.
func TestEffectRunsOnBehalfOfItsPrincipal(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<CreateVpcResponse><vpc><vpcId>vpc-1</vpcId></vpc></CreateVpcResponse>`)
	}))
	defer srv.Close()
	in := facade.EffectInput{Mutation: []byte(`{"CidrBlock":"10.0.0.0/16"}`), Params: map[string]string{"region": "us-east-1"}}

	eff, addr := fixtureAs(t, srv, "bob", onlyFor("alice"))
	if _, err := eff.Effect(context.Background(), addr+":insert", "demo/aws/ec2/vpc", in); err == nil || calls != 0 {
		t.Errorf("bob: err = %v, calls = %d; want a refusal before the wire", err, calls)
	}
	eff, addr = fixtureAs(t, srv, "alice", onlyFor("alice"))
	if _, err := eff.Effect(context.Background(), addr+":insert", "demo/aws/ec2/vpc", in); err != nil || calls != 1 {
		t.Errorf("alice: err = %v, calls = %d; want one create", err, calls)
	}
}
