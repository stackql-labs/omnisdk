package docsem_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/docsem"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

// The document already states which operation creates a resource, which deletes it and which reads
// it. Deriving those is what replaces the hand-authored table written once per provider.
func TestDeriveEC2FromDocuments(t *testing.T) {
	reg, err := stackqldoc.OpenRegistry(os.DirFS("../../pkg/docparse/stackqldoc/testdata"))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	cat, err := reg.Catalog("aws")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}

	resources, err := docsem.Derive(cat, "ec2", "vpcs", "subnets")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	byPath := map[string]docsem.Resource{}
	for _, r := range resources {
		byPath[r.Path()] = r
	}

	// The VPC case derives exactly what was otherwise hand-written.
	vpc, ok := byPath["aws.ec2.vpcs"]
	if !ok {
		t.Fatal("aws.ec2.vpcs not derived")
	}
	if vpc.Create() != "GET_CreateVpc" || vpc.Read() != "GET_DescribeVpcs" || vpc.Delete() != "GET_DeleteVpc" {
		t.Errorf("vpcs = create %s / read %s / delete %s, want CreateVpc / DescribeVpcs / DeleteVpc",
			vpc.Create(), vpc.Read(), vpc.Delete())
	}

	// The subnet case does not, and the failure is the point: the document binds two creates, and
	// first-wins picks CreateDefaultSubnet. Derivation yields candidates, and a resource with more
	// than one operation for a verb needs the caller to say which.
	subnet := byPath["aws.ec2.subnets"]
	if subnet.Create() == "GET_CreateSubnet" {
		t.Log("subnets now derives CreateSubnet; the ambiguity note can go")
	}

	// No update path is derived, so drift is refused rather than dispatched to whatever operation
	// the document happens to map to the update verb.
	for _, d := range docsem.Declarations(resources) {
		if d.Update != "" {
			t.Errorf("%s derived an update path %q; updates are not derivable", d.Exchange, d.Update)
		}
	}
}

// An operation the document describes incompletely is reported, not tolerated silently. The engine
// can still run it — an undocumented response is assumed empty and answered with the status — but a
// caller weighing whether a resource is safely manageable needs to see which operations are thin.
func TestOutliersReportsThinlyDescribedOperations(t *testing.T) {
	reg, err := stackqldoc.OpenRegistry(os.DirFS("../../pkg/docparse/stackqldoc/testdata"))
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

	found, err := docsem.Outliers(cat, addr)
	if err != nil {
		t.Fatalf("outliers: %v", err)
	}
	for _, o := range found {
		t.Logf("%s %s %s: %s", o.Path(), o.Verb(), o.Operation(), o.Reason())
	}
	// The delete is the known case: its reply is undocumented, which is why compiling it needs the
	// response-decode tolerance.
	var sawDelete bool
	for _, o := range found {
		if o.Verb() == "delete" {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Error("delete not reported as an outlier; its response is undocumented")
	}
}
