package docsem_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/docsem"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

// A verb fans out and the inputs pick which operation applies: only CreateSubnet accepts VpcId and
// CidrBlock, so supplying them selects it over CreateDefaultSubnet regardless of declaration order.
func TestSelectPicksBySignature(t *testing.T) {
	reg, err := stackqldoc.OpenRegistry(os.DirFS("../../pkg/docparse/stackqldoc/testdata"))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	cat, err := reg.Catalog("aws")
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	// The catalog's addresses carry the document's own provider name, prefix included.
	var addr string
	for _, p := range cat.Paths() {
		if strings.HasSuffix(p, ".ec2.subnets") {
			addr = p
			break
		}
	}
	if addr == "" {
		t.Fatal("ec2.subnets not addressable")
	}

	op, err := docsem.Select(cat, addr, "insert", []string{"VpcId", "CidrBlock"})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if op.OperationID() != "GET_CreateSubnet" {
		t.Errorf("operation = %s, want GET_CreateSubnet", op.OperationID())
	}

	// The other create is reachable by its own signature.
	// AvailabilityZone alone also lands on CreateSubnet, which declares it too: where several
	// operations match, declaration order breaks the tie, exactly as the document's own resolution
	// does.
	def, err := docsem.Select(cat, addr, "insert", []string{"AvailabilityZone"})
	if err != nil {
		t.Fatalf("select default: %v", err)
	}
	if def.OperationID() != "GET_CreateSubnet" {
		t.Errorf("AvailabilityZone selected %s, want the first declaration that accepts it", def.OperationID())
	}

	// An input no operation declares is an error, not a silent pick.
	if _, err := docsem.Select(cat, addr, "insert", []string{"NotAThing"}); err == nil {
		t.Error("unknown input selected an operation; want an error")
	}
}
