package docrun_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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
func fixture(t *testing.T, srv *httptest.Server) (facade.Effector, string) {
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
	eff := docrun.New(cat, dslReg,
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
