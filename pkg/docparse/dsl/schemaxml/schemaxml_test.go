package schemaxml_test

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/schemaxml"
)

// schema is a hand-built stand-in for what a parsed document supplies.
type schema struct {
	props map[string]*schema
	items *schema
	wire  string
}

func (s *schema) Property(name string) (schemaxml.Schema, bool) {
	p, ok := s.props[name]
	if !ok {
		return nil, false
	}
	return p, true
}

func (s *schema) Properties() []string {
	out := make([]string, 0, len(s.props))
	for k := range s.props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *schema) Items() (schemaxml.Schema, bool) {
	if s.items == nil {
		return nil, false
	}
	return s.items, true
}

func (s *schema) WireName() string { return s.wire }

// vpcs mirrors the EC2 document: line_items is an array of a row type whose properties carry the
// element names the wire actually uses.
func vpcs() *schema {
	row := &schema{props: map[string]*schema{
		"VpcId":     {wire: "vpcId"},
		"CidrBlock": {wire: "cidrBlock"},
		"OwnerId":   {wire: "ownerId"},
	}}
	return &schema{props: map[string]*schema{"line_items": {items: row}}}
}

func run(t *testing.T, body string, ctx dsl.Context) map[string]any {
	t.Helper()
	out, err := schemaxml.New().Eval(ctx, "", []byte(body))
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// The transform is the inverse of the schema's element-name annotations, plus a projection onto the
// declared properties: the wire says vpcId, the row says VpcId.
func TestListIsProjectedOntoSchemaNames(t *testing.T) {
	got := run(t, `<DescribeVpcsResponse><vpcSet>
		<item><vpcId>vpc-1</vpcId><cidrBlock>10.0.0.0/16</cidrBlock><undeclared>x</undeclared></item>
		<item><vpcId>vpc-2</vpcId><cidrBlock>10.1.0.0/16</cidrBlock></item>
	</vpcSet></DescribeVpcsResponse>`, dsl.Context{Schema: vpcs(), ListProperty: "line_items", Protocol: "ec2"})

	rows, _ := got["line_items"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want 2", got)
	}
	first, _ := rows[0].(map[string]any)
	if first["VpcId"] != "vpc-1" || first["CidrBlock"] != "10.0.0.0/16" {
		t.Errorf("row = %v, want the schema's property names", first)
	}
	// A field the schema does not declare is dropped: the schema is the contract, not whatever the
	// provider happened to send.
	if _, present := first["undeclared"]; present {
		t.Errorf("row = %v, want undeclared fields dropped", first)
	}
}

// A set with one member decodes as a single element, not a slice. It is still one row.
func TestSingleItemIsStillAList(t *testing.T) {
	got := run(t, `<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-1</vpcId></item></vpcSet></DescribeVpcsResponse>`,
		dsl.Context{Schema: vpcs(), ListProperty: "line_items", Protocol: "ec2"})

	rows, _ := got["line_items"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want 1", got)
	}
}

// A reply about one object has no set at all: the payload is the row.
func TestSingletonPayloadIsTheRow(t *testing.T) {
	got := run(t, `<CreateVpcResponse><vpc><vpcId>vpc-9</vpcId></vpc></CreateVpcResponse>`,
		dsl.Context{Schema: vpcs(), ListProperty: "line_items", Protocol: "ec2"})

	rows, _ := got["line_items"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want the wrapped object as one row", got)
	}
	if rows[0].(map[string]any)["VpcId"] != "vpc-9" {
		t.Errorf("row = %v, want vpc-9", rows[0])
	}
}

// Scalars beside the list belong to the response, not to a row — dropping a pagination token would
// lose the next page.
func TestScalarSiblingsPassThrough(t *testing.T) {
	got := run(t, `<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-1</vpcId></item></vpcSet><nextToken>abc</nextToken></DescribeVpcsResponse>`,
		dsl.Context{Schema: vpcs(), ListProperty: "line_items", Protocol: "ec2"})

	if got["nextToken"] != "abc" {
		t.Errorf("got %v, want the pagination token retained", got)
	}
}

// An identifier that is all digits must survive as those digits: decoding through a numeric type
// rewrites anything past float64's exact range, and an id lands there.
func TestNumericIdentifiersKeepTheirDigits(t *testing.T) {
	got := run(t, `<DescribeVpcsResponse><vpcSet><item><vpcId>vpc-1</vpcId><ownerId>10000000000000001</ownerId></item></vpcSet></DescribeVpcsResponse>`,
		dsl.Context{Schema: vpcs(), ListProperty: "line_items", Protocol: "ec2"})

	rows, _ := got["line_items"].([]any)
	if owner := rows[0].(map[string]any)["OwnerId"]; owner != "10000000000000001" {
		t.Errorf("ownerId = %v, want the digits unchanged", owner)
	}
}

// The transform IS the schema applied, so no schema is a failure — not an empty result that would
// read as "the provider returned nothing".
func TestMissingSchemaIsAnError(t *testing.T) {
	if _, err := schemaxml.New().Eval(dsl.Context{ListProperty: "line_items"}, "", []byte(`<a/>`)); err == nil {
		t.Error("eval with no schema succeeded, want an error")
	}
	if _, err := schemaxml.New().Eval(dsl.Context{Schema: vpcs()}, "", []byte(`<a/>`)); err == nil {
		t.Error("eval with no list property succeeded, want an error")
	}
	if _, err := schemaxml.New().Eval(dsl.Context{Schema: vpcs(), ListProperty: "line_items"}, "{{ . }}", []byte(`<a/>`)); err == nil {
		t.Error("eval with a program succeeded; this language has no text")
	}
}

// The query protocol (IAM) nests rows in <ActionResult> beside <ResponseMetadata>, with <member>
// items and the truncation flag as a sibling of the list.
func TestQueryProtocolResultAndMembers(t *testing.T) {
	row := &schema{props: map[string]*schema{"UserName": {}, "UserId": {}}}
	users := &schema{props: map[string]*schema{"line_items": {items: row}}}
	got := run(t, `<ListUsersResponse><ListUsersResult><Users>`+
		`<member><UserName>alice</UserName></member><member><UserName>bob</UserName></member>`+
		`</Users><IsTruncated>false</IsTruncated></ListUsersResult>`+
		`<ResponseMetadata><RequestId>r-1</RequestId></ResponseMetadata></ListUsersResponse>`,
		dsl.Context{Schema: users, ListProperty: "line_items", Protocol: "query"})

	rows, _ := got["line_items"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want alice and bob", got["line_items"])
	}
	if got["IsTruncated"] != "false" {
		t.Errorf("got %v, want the truncation flag retained", got)
	}
}
