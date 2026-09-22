package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

const vpcsAddr = "stackql_unstable_aws.ec2.vpcs"

// ec2Stub answers DescribeVpcs with one VPC whose tag value is a delimited string, which is what
// gives a function something to do. Subnets answer with one subnet per VPC id seen.
func ec2Stub(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("Action")
		*seen = append(*seen, action+"|"+r.URL.Query().Get("Filter.1.Value.1"))
		w.Header().Set("Content-Type", "text/xml")
		switch action {
		case "DescribeVpcs":
			fmt.Fprint(w, `<DescribeVpcsResponse><vpcSet>`+
				`<item><vpcId>vpc-1</vpcId><cidrBlock>10.0.0.0/16</cidrBlock></item>`+
				`<item><vpcId>vpc-2</vpcId><cidrBlock>10.1.0.0/24</cidrBlock></item>`+
				`</vpcSet></DescribeVpcsResponse>`)
		default:
			fmt.Fprint(w, `<DescribeSubnetsResponse><subnetSet><item><subnetId>subnet-1</subnetId></item></subnetSet></DescribeSubnetsResponse>`)
		}
	}))
}

func runGraph(t *testing.T, srv *httptest.Server, g omnisdk.Graph) []omnisdk.Row {
	t.Helper()
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
	return got
}

func awsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
}

// A scalar function in the select list computes a column the document never returned, and the
// select REPLACES the row — a field it does not name is not emitted.
func TestScalarFunctionProjection(t *testing.T) {
	requireCorpus(t)
	awsEnv(t)
	var seen []string
	srv := ec2Stub(t, &seen)
	defer srv.Close()

	p, err := omnisdk.NewProjection(vpcsAddr, []omnisdk.SelectColumn{
		omnisdk.NewSelectColumn("vpc", omnisdk.NewField("VpcId")),
		omnisdk.NewSelectColumn("prefix", omnisdk.NewCall("split_part",
			omnisdk.NewField("CidrBlock"), omnisdk.NewLiteral("/"), omnisdk.NewLiteral(1))),
		omnisdk.NewSelectColumn("mask", omnisdk.NewCall("split_part",
			omnisdk.NewField("CidrBlock"), omnisdk.NewLiteral("/"), omnisdk.NewLiteral(2))),
	})
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	g, err := omnisdk.NewGraphWithProjections([]string{vpcsAddr}, nil, []omnisdk.Projection{p})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}

	got := runGraph(t, srv, g)
	if len(got) != 2 {
		t.Fatalf("rows = %#v, want 2", got)
	}
	if got[0]["vpc"] != "vpc-1" || got[0]["prefix"] != "10.0.0.0" || got[0]["mask"] != "16" {
		t.Errorf("row 0 = %#v", got[0])
	}
	if got[1]["mask"] != "24" {
		t.Errorf("row 1 = %#v", got[1])
	}
	if _, present := got[0]["CidrBlock"]; present {
		t.Errorf("select replaces the row; CidrBlock should not survive: %#v", got[0])
	}
}

// A row-producing function fans one provider row out into several, carrying the scalar columns of
// the row it came from onto each.
func TestTableValuedFunctionProjection(t *testing.T) {
	requireCorpus(t)
	awsEnv(t)
	var seen []string
	srv := ec2Stub(t, &seen)
	defer srv.Close()

	p, err := omnisdk.NewProjection(vpcsAddr, []omnisdk.SelectColumn{
		omnisdk.NewSelectColumn("vpc", omnisdk.NewField("VpcId")),
		omnisdk.NewSelectColumn("octet", omnisdk.NewCall("string_to_table",
			omnisdk.NewCall("split_part", omnisdk.NewField("CidrBlock"), omnisdk.NewLiteral("/"), omnisdk.NewLiteral(1)),
			omnisdk.NewLiteral("."))),
	})
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	g, err := omnisdk.NewGraphWithProjections([]string{vpcsAddr}, nil, []omnisdk.Projection{p})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}

	got := runGraph(t, srv, g)
	// Two VPCs, four octets each.
	if len(got) != 8 {
		t.Fatalf("rows = %d, want 8: %#v", len(got), got)
	}
	if got[0]["vpc"] != "vpc-1" || got[0]["octet"] != "10" {
		t.Errorf("row 0 = %#v", got[0])
	}
	if got[1]["octet"] != "0" {
		t.Errorf("row 1 = %#v", got[1])
	}
}

// Two row-producing columns would be a cross product. A join is stated as one, not smuggled into a
// select list, so this is refused when the plan is built.
func TestSelectRefusesTwoRowProducingColumns(t *testing.T) {
	requireCorpus(t)
	awsEnv(t)
	p, err := omnisdk.NewProjection(vpcsAddr, []omnisdk.SelectColumn{
		omnisdk.NewSelectColumn("a", omnisdk.NewCall("string_to_table", omnisdk.NewField("VpcId"), omnisdk.NewLiteral("-"))),
		omnisdk.NewSelectColumn("b", omnisdk.NewCall("string_to_table", omnisdk.NewField("CidrBlock"), omnisdk.NewLiteral("."))),
	})
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	g, err := omnisdk.NewGraphWithProjections([]string{vpcsAddr}, nil, []omnisdk.Projection{p})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if _, err := omnisdk.NewGraphQuery(corpus, g, omnisdk.Args{Params: map[string]string{"region": "us-east-1"}}); err == nil {
		t.Fatal("want a refusal for two row-producing columns, got none")
	}
}

const subnetsAddr = "stackql_unstable_aws.ec2.subnets"

// A projection is applied BEFORE the row travels an edge, so a join may be on a value a function
// computed rather than one the document returned.
func TestJoinOnAScalarFunctionResult(t *testing.T) {
	requireCorpus(t)
	awsEnv(t)
	var seen []string
	srv := ec2Stub(t, &seen)
	defer srv.Close()

	p, err := omnisdk.NewProjection(vpcsAddr, []omnisdk.SelectColumn{
		omnisdk.NewSelectColumn("VpcId", omnisdk.NewField("VpcId")),
		omnisdk.NewSelectColumn("prefix", omnisdk.NewCall("split_part",
			omnisdk.NewField("CidrBlock"), omnisdk.NewLiteral("/"), omnisdk.NewLiteral(1))),
	})
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	g, err := omnisdk.NewGraphWithProjections(
		[]string{vpcsAddr, subnetsAddr},
		[]omnisdk.Wiring{omnisdk.NewWiring(subnetsAddr,
			[]omnisdk.Inbound{omnisdk.NewInbound(vpcsAddr, "prefix", "prefix")},
			"golang_template_json_v0.1.0",
			`{"Filter.1.Name":"cidr-block","Filter.1.Value.1":"{{ .prefix }}"}`,
			"Filter.1.Name", "Filter.1.Value.1")},
		[]omnisdk.Projection{p},
	)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}

	if rows := runGraph(t, srv, g); len(rows) == 0 {
		t.Fatal("no rows")
	}
	// The filter carries the computed value, not the field the document returned.
	var filtered []string
	for _, s := range seen {
		if len(s) > 16 && s[:16] == "DescribeSubnets|" {
			filtered = append(filtered, s[16:])
		}
	}
	want := map[string]bool{"10.0.0.0": true, "10.1.0.0": true}
	if len(filtered) != 2 || !want[filtered[0]] || !want[filtered[1]] {
		t.Errorf("subnet filters = %v, want the two computed prefixes", filtered)
	}
}

// A row-producing function on the producing side fans the row out BEFORE the edge, so each produced
// row drives its own downstream call.
func TestJoinOnATableValuedFunctionResult(t *testing.T) {
	requireCorpus(t)
	awsEnv(t)
	var seen []string
	srv := ec2Stub(t, &seen)
	defer srv.Close()

	p, err := omnisdk.NewProjection(vpcsAddr, []omnisdk.SelectColumn{
		omnisdk.NewSelectColumn("VpcId", omnisdk.NewField("VpcId")),
		omnisdk.NewSelectColumn("octet", omnisdk.NewCall("string_to_table",
			omnisdk.NewCall("split_part", omnisdk.NewField("CidrBlock"), omnisdk.NewLiteral("/"), omnisdk.NewLiteral(1)),
			omnisdk.NewLiteral("."))),
	})
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	g, err := omnisdk.NewGraphWithProjections(
		[]string{vpcsAddr, subnetsAddr},
		[]omnisdk.Wiring{omnisdk.NewWiring(subnetsAddr,
			[]omnisdk.Inbound{omnisdk.NewInbound(vpcsAddr, "octet", "octet")},
			"golang_template_json_v0.1.0",
			`{"Filter.1.Name":"tag:octet","Filter.1.Value.1":"{{ .octet }}"}`,
			"Filter.1.Name", "Filter.1.Value.1")},
		[]omnisdk.Projection{p},
	)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}

	if rows := runGraph(t, srv, g); len(rows) == 0 {
		t.Fatal("no rows")
	}
	calls := 0
	for _, s := range seen {
		if len(s) > 16 && s[:16] == "DescribeSubnets|" {
			calls++
		}
	}
	// Two VPCs, four octets each: the fan-out drives eight calls, not two.
	if calls != 8 {
		t.Errorf("subnet calls = %d, want 8 (one per produced row)", calls)
	}
}
