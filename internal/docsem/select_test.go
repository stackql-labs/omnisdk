package docsem_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/analyze"
	"github.com/stackql-labs/omnisdk/internal/docsem"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

// A verb fans out and the inputs pick which operation applies: only CreateSubnet accepts VpcId and
// CidrBlock, so supplying them selects it over CreateDefaultSubnet regardless of declaration order.
func TestCapabilitiesSelectBySignature(t *testing.T) {
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
	an := analyze.New(docsem.Capabilities(cat), analyze.PermitAll())
	choose := func(inputs ...string) (string, error) {
		m, err := an.Analyze(context.Background(), analyze.NewStatement(addr, "insert", inputs, inputs, ""))
		if err != nil {
			return "", err
		}
		ex, ok := docsem.Exchange(m)
		if !ok {
			t.Fatalf("method %s carries no document operation", m.Name())
		}
		return ex.OperationID(), nil
	}

	op, err := choose("VpcId", "CidrBlock")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if op != "GET_CreateSubnet" {
		t.Errorf("operation = %s, want GET_CreateSubnet", op)
	}

	// The other create is reachable by its own signature.
	def, err := choose("AvailabilityZone")
	if err != nil {
		t.Fatalf("select default: %v", err)
	}
	if def != "GET_CreateDefaultSubnet" {
		t.Errorf("AvailabilityZone selected %s, want GET_CreateDefaultSubnet", def)
	}

	// An input no operation declares is an error, not a silent pick.
	if _, err := choose("NotAThing"); err == nil {
		t.Error("unknown input selected an operation; want an error")
	}
}
