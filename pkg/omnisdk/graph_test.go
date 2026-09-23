package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

const corpus = "../../test/corpus/registry"

// requireCorpus skips a test that needs the provider-document corpus, or FAILS it where the corpus
// is meant to be there.
//
// The corpus is a vendored subset of an external registry and is not tracked, so a clean clone does
// not have it: skipping says what is absent and why, rather than reporting a missing fixture as a
// broken build. But a silent skip in CI is worse than either — it hides a regression behind a green
// run — so CI sets OMNISDK_REQUIRE_CORPUS after populating it, and absence is then a failure.
func requireCorpus(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(corpus); err == nil {
		return
	}
	const how = "run ./test/corpus/fetch.sh (see the developer guide)"
	if os.Getenv("OMNISDK_REQUIRE_CORPUS") != "" {
		t.Fatalf("provider-document corpus absent at %s and OMNISDK_REQUIRE_CORPUS is set; %s", corpus, how)
	}
	t.Skipf("provider-document corpus absent at %s; %s", corpus, how)
}

// A document describes one provider's calls and does not state that a subnet belongs to a VPC. The
// query says so: the VPC's id is bound into the subnet describe, which is a β edge nobody wrote down.
func TestGraphJoinsTwoExchangesTheDocumentDoesNotRelate(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("Action")
		actions = append(actions, action+"|"+r.URL.Query().Get("Filter.1.Name")+"="+r.URL.Query().Get("Filter.1.Value.1"))
		w.Header().Set("Content-Type", "text/xml")
		switch action {
		case "DescribeVpcs":
			fmt.Fprint(w, `<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-1</vpcId></item></vpcSet></DescribeVpcsResponse>`)
		default:
			fmt.Fprint(w, `<DescribeSubnetsResponse><subnetSet><item><subnetId>subnet-1</subnetId></item></subnetSet></DescribeSubnetsResponse>`)
		}
	}))
	defer srv.Close()

	const vpcs = "stackql_unstable_aws.ec2.vpcs"
	const subnets = "stackql_unstable_aws.ec2.subnets"
	// DescribeSubnets declares no VpcId — its parameters are Filter, SubnetId, NextToken,
	// MaxResults, DryRun. So the value needs shaping on the way in, which is what T_in is for.
	// No overrides: the documents declare a row path of "$.line_items", the shape the schema-driven
	// transform produces, and the engine now implements it. Row fields carry the SCHEMA's names —
	// VpcId, not the wire's vpcId — because projection is what that transform does.
	g, err := omnisdk.NewGraph(
		[]string{vpcs, subnets},
		[]omnisdk.Wiring{omnisdk.NewWiring(subnets,
			[]omnisdk.Inbound{omnisdk.NewInbound(vpcs, "VpcId", "vpc_id")},
			gotemplate.TypeJSON1,
			`{"Filter.1.Name":"vpc-id","Filter.1.Value.1":"{{ .vpc_id }}"}`,
			"Filter.1.Name", "Filter.1.Value.1",
		)},
	)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}

	pl, err := omnisdk.NewGraphQuery(corpus, g, omnisdk.Args{
		Endpoint: srv.URL,
		Params:   map[string]string{"region": "us-east-1"},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rows.Close()

	var got []omnisdk.Row
	for rows.Next() {
		got = append(got, rows.Row())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	joined := strings.Join(actions, " ")
	if !strings.Contains(joined, "DescribeVpcs") || !strings.Contains(joined, "DescribeSubnets") {
		t.Fatalf("actions = %v, want both exchanges run", actions)
	}
	// The edge is what makes this a join rather than two independent calls, and T_in is what makes
	// the edge usable: the id became the filter expression the subnet API actually accepts.
	if !strings.Contains(joined, "vpc-id=vpc-1") {
		t.Errorf("actions = %v, want the subnet describe filtered by vpc-1", actions)
	}
	if len(got) == 0 {
		t.Error("no rows")
	}
}

// An edge naming an exchange the query does not run is caught where it can name the address, not
// as a missing binding at execution.
func TestGraphRejectsAJoinOntoAnAbsentAddress(t *testing.T) {
	_, err := omnisdk.NewGraph(
		[]string{"stackql_unstable_aws.ec2.vpcs"},
		[]omnisdk.Wiring{omnisdk.NewWiring("elsewhere.ec2.subnets",
			[]omnisdk.Inbound{omnisdk.NewInbound("stackql_unstable_aws.ec2.vpcs", "vpcId", "")}, "", "")},
	)
	if err == nil {
		t.Error("wiring onto an address the graph excludes was accepted")
	}
}
