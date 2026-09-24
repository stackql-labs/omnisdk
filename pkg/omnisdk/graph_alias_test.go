package omnisdk_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// vpcChainStub answers an unfiltered DescribeVpcs with vpc-1 and vpc-2, and a filtered one with a
// single VPC named after the filter — so a value's origin is visible in the next request. Each
// request is recorded as "<signing region>|<filter>".
func vpcChainStub(t *testing.T, seen *calls) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filter := r.URL.Query().Get("Filter.1.Value.1")
		seen.add(signingRegion(r.Header.Get("Authorization")) + "|" + filter)
		w.Header().Set("Content-Type", "text/xml")
		if filter == "" {
			fmt.Fprint(w, `<DescribeVpcsResponse><vpcSet>`+
				`<item><vpcId>vpc-1</vpcId></item><item><vpcId>vpc-2</vpcId></item>`+
				`</vpcSet></DescribeVpcsResponse>`)
			return
		}
		fmt.Fprintf(w, `<DescribeVpcsResponse><vpcSet><item><vpcId>child-of-%s</vpcId></item></vpcSet></DescribeVpcsResponse>`, filter)
	}))
}

// signingRegion reads the region out of a SigV4 credential scope: Credential=AK/date/REGION/svc/...
func signingRegion(auth string) string {
	_, cred, ok := strings.Cut(auth, "Credential=")
	if !ok {
		return ""
	}
	parts := strings.Split(cred, "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

func byVpcID(from string) omnisdk.Inbound { return omnisdk.NewInbound(from, "VpcId", "vpc_id") }

func filteredBy(to string, in omnisdk.Inbound) omnisdk.Wiring {
	return omnisdk.NewWiring(to, []omnisdk.Inbound{in}, gotemplate.TypeJSON1,
		`{"Filter.1.Name":"vpc-id","Filter.1.Value.1":"{{ .vpc_id }}"}`,
		"Filter.1.Name", "Filter.1.Value.1")
}

// One address referenced three times. c's edge names a, so c must be filtered by a's ids — not by
// b's, which share the attribute name and merge into the row later.
func TestSelfJoinEdgeReadsTheNodeItNames(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	var seen calls
	srv := vpcChainStub(t, &seen)
	defer srv.Close()

	g, err := omnisdk.NewGraph(
		[]omnisdk.Node{
			omnisdk.NewNode("a", vpcsAddr, nil),
			omnisdk.NewNode("b", vpcsAddr, nil),
			omnisdk.NewNode("c", vpcsAddr, nil),
		},
		[]omnisdk.Wiring{filteredBy("b", byVpcID("a")), filteredBy("c", byVpcID("a"))},
	)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	rows := runGraph(t, srv, g)

	var filters []string
	for _, c := range seen.matching("") {
		_, f, _ := strings.Cut(c, "|")
		filters = append(filters, f)
	}
	sort.Strings(filters)
	want := []string{"", "vpc-1", "vpc-1", "vpc-2", "vpc-2"}
	if !slices.Equal(filters, want) {
		t.Errorf("filters = %v, want %v: an edge read a value its producer did not emit", filters, want)
	}
	if len(rows) != 2 {
		t.Errorf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		for k := range r {
			if strings.HasPrefix(k, "\x00") {
				t.Errorf("row leaks internal key %q", k)
			}
		}
	}
}

// One address read in two regions: each reference is its own exchange with its own inputs.
func TestSameAddressTwiceWithDifferentParams(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	var seen calls
	srv := vpcChainStub(t, &seen)
	defer srv.Close()

	g, err := omnisdk.NewGraph(
		[]omnisdk.Node{
			omnisdk.NewNode("east", vpcsAddr, map[string]string{"region": "us-east-1"}),
			omnisdk.NewNode("west", vpcsAddr, map[string]string{"region": "us-west-2"}),
		},
		nil,
	)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	runGraph(t, srv, g)

	// Unwired nodes run as a nested loop, so west is called once per east row; what matters here is
	// that every call is signed into its own reference's region and both regions are reached.
	got := slices.Compact(slices.Sorted(slices.Values(seen.matching(""))))
	if want := []string{"us-east-1|", "us-west-2|"}; !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestGraphRejectsAmbiguousNodes(t *testing.T) {
	cases := map[string]struct {
		nodes   []omnisdk.Node
		wirings []omnisdk.Wiring
	}{
		"duplicate alias": {
			nodes: []omnisdk.Node{omnisdk.NewNode("a", vpcsAddr, nil), omnisdk.NewNode("a", subnetsAddr, nil)},
		},
		"same address twice, unaliased": {
			nodes: []omnisdk.Node{omnisdk.NewNode("", vpcsAddr, nil), omnisdk.NewNode("", vpcsAddr, nil)},
		},
		"no alias and no address": {
			nodes: []omnisdk.Node{omnisdk.NewNode("", "", nil)},
		},
		"edge onto itself": {
			nodes:   []omnisdk.Node{omnisdk.NewNode("a", vpcsAddr, nil)},
			wirings: []omnisdk.Wiring{filteredBy("a", byVpcID("a"))},
		},
		"two wirings into one node": {
			nodes: []omnisdk.Node{
				omnisdk.NewNode("a", vpcsAddr, nil), omnisdk.NewNode("b", vpcsAddr, nil), omnisdk.NewNode("c", vpcsAddr, nil),
			},
			wirings: []omnisdk.Wiring{filteredBy("c", byVpcID("a")), filteredBy("c", byVpcID("b"))},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := omnisdk.NewGraph(tc.nodes, tc.wirings); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// An unaliased reference is named by its address, as in SQL.
func TestUnaliasedNodeIsItsAddress(t *testing.T) {
	g, err := omnisdk.NewGraph(
		[]omnisdk.Node{omnisdk.NewNode("", vpcsAddr, nil), omnisdk.NewNode("s", subnetsAddr, nil)},
		[]omnisdk.Wiring{filteredBy("s", byVpcID(vpcsAddr))},
	)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if got := g.Nodes()[0].Alias(); got != vpcsAddr {
		t.Errorf("alias = %q, want the address", got)
	}
}
