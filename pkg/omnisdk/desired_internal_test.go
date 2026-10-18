package omnisdk

import (
	"strings"
	"testing"
)

// The route a drift takes on an EC2 VPC, from the document alone: the one update operation that
// accepts what changed, sent only what changed; nothing where no operation accepts the change or a
// field is to be removed through an operation with no body.
func TestDriftRouteOnAVPC(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	if _, err := openRegistry("../../test/corpus/registry"); err != nil {
		t.Skipf("corpus: %v", err)
	}
	vpc := NewResource("aws/ec2/vpc", "aws", "ec2.vpcs", []byte(`{"CidrBlock":"10.0.0.0/16","InstanceTenancy":"default"}`),
		nil, nil, "", "", "line_items.VpcId", "VpcId", "")
	pl, err := newDesiredPlan("../../test/corpus/registry", "demo", t.TempDir(), "r1", []ManagedResource{vpc},
		Args{Params: map[string]string{"region": "us-east-1"}})
	if err != nil {
		t.Fatal(err)
	}
	p := pl.(*desiredPlan)
	route := p.route(vpc, p.targets["aws/ec2/vpc"], nil)

	method, send, err := route(map[string]any{"CidrBlock": "10.0.0.0/16", "InstanceTenancy": "default"},
		map[string]any{"CidrBlock": "10.0.0.0/16", "InstanceTenancy": "dedicated"})
	if err != nil || method != "modify_vpc_tenancy" || len(send) != 1 || send["InstanceTenancy"] != "default" {
		t.Errorf("tenancy drift: %s %v %v; want modify_vpc_tenancy sent only the tenancy", method, send, err)
	}
	for name, c := range map[string]struct{ mutation, live map[string]any }{
		"unaccepted field": {map[string]any{"EnableDnsSupport": "true"}, map[string]any{"EnableDnsSupport": "false"}},
		"unset field":      {map[string]any{"InstanceTenancy": nil}, map[string]any{"InstanceTenancy": "dedicated"}},
	} {
		if m, _, err := route(c.mutation, c.live); err == nil {
			t.Errorf("%s: routed to %s; want a refusal", name, m)
		} else if name == "unset field" && !strings.Contains(err.Error(), "cannot remove") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
