package stackqldoc_test

import (
	"os"
	"runtime"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

func mb(b uint64) float64 { return float64(b) / 1024 / 1024 }

// A query spans several services and several providers. What must scale with it is the number of
// EXCHANGES, not the documents they were resolved from.
func TestWorkingSetIsExchangesNotDocuments(t *testing.T) {
	c, err := stackqldoc.Open(os.DirFS("testdata/aws/v00.00.00000"))
	if err != nil {
		t.Fatal(err)
	}
	addrs := []string{
		"stackql_unstable_aws.ec2.instances", "stackql_unstable_aws.ec2.security_groups",
		"stackql_unstable_aws.ec2.volumes", "stackql_unstable_aws.s3.buckets",
	}

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)

	var held []aot.AOTExchange
	for _, a := range addrs {
		ex, err := c.Exchange(a)
		if err != nil {
			t.Logf("skip %s: %v", a, err)
			continue
		}
		held = append(held, ex)
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)

	retained := mb(m1.HeapAlloc - m0.HeapAlloc)
	t.Logf("%d exchanges resolved across services: %.2f MB retained (%.0f KB each)",
		len(held), retained, retained*1024/float64(len(held)))
	if retained > 2 {
		t.Fatalf("holding %d exchanges retained %.2f MB — documents are outliving planning", len(held), retained)
	}
	runtime.KeepAlive(held)
	runtime.KeepAlive(c)
}
