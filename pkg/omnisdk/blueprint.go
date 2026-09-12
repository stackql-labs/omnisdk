package omnisdk

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/stackql-labs/omnisdk/internal/effect/awsec2"
	"github.com/stackql-labs/omnisdk/internal/kind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Blueprint is a precanned deployment addressable by handle. A client names the handle and supplies
// inputs; the blueprint turns those into the resources a run converges.
//
// It is discovery in the same shape as the query catalog: list the handles, read one's parameters,
// then invoke it. A client never assembles provider wiring — naming the handle is the whole request,
// and idempotence, ordering, locking and compensation are the system's problem.
type Blueprint interface {
	// Handle addresses the blueprint, e.g. "aws-vpc-subnet".
	Handle() string
	Summary() string
	// Params are the inputs it accepts, in the same DTO the query catalog publishes.
	Params() []Param
	// Resources renders the deployment. Unknown inputs are rejected rather than ignored: a mistyped
	// parameter that silently does nothing is worse than one that fails.
	Resources(inputs map[string]string) ([]ManagedResource, error)
}

// Blueprints lists every handle, sorted.
func Blueprints() []Blueprint {
	out := slices.Collect(maps.Values(blueprints))
	sort.Slice(out, func(i, j int) bool { return out[i].Handle() < out[j].Handle() })
	return out
}

// BlueprintFor resolves one handle.
func BlueprintFor(handle string) (Blueprint, bool) {
	b, ok := blueprints[handle]
	return b, ok
}

var blueprints = map[string]Blueprint{
	"aws-vpc-subnet": awsVpcSubnet{},
}

// blueprintBase gives every blueprint the same input handling, so a new one declares parameters and
// renders resources and nothing else.
type blueprintBase struct{}

// kinds maps a published kind name to its behaviour. A param declares the name; this resolves it,
// so discovery and validation cannot disagree about what a param accepts.
var kinds = map[string]facade.Kind{
	"string":  kind.String(),
	"int":     kind.Int(),
	"decimal": kind.Decimal(),
	"bool":    kind.Bool(),
	"object":  kind.Object(),
}

// check rejects unknown inputs, reports missing required ones, and parses each supplied value with
// its param's kind — so a malformed value fails here, naming the param, rather than somewhere
// downstream as a wire error or, worse, silently.
func check(params []Param, inputs map[string]string) error {
	known := make(map[string]Param, len(params))
	for _, p := range params {
		known[p.Name] = p
	}
	for name := range inputs {
		if _, ok := known[name]; !ok {
			return fmt.Errorf("omnisdk: unknown input %q", name)
		}
	}
	for _, p := range params {
		v, given := inputs[p.Name]
		if !given || v == "" {
			if p.Required {
				return fmt.Errorf("omnisdk: %s is required", p.Name)
			}
			continue
		}
		k, ok := kinds[p.Type.Kind]
		if !ok {
			// An undeclared kind is not a licence to accept anything: guessing is what puts a
			// value nobody wrote onto the wire.
			return fmt.Errorf("omnisdk: %s declares no usable kind %q", p.Name, p.Type.Kind)
		}
		if _, err := k.Parse([]byte(v)); err != nil {
			return fmt.Errorf("omnisdk: %s: %w", p.Name, err)
		}
	}
	return nil
}

// tagsOf parses a tag input: a JSON object of string keys to string values, empty meaning none.
func tagsOf(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	var tags map[string]string
	if err := json.Unmarshal([]byte(s), &tags); err != nil {
		return nil, fmt.Errorf("omnisdk: tags must be a JSON object of strings: %w", err)
	}
	return tags, nil
}

// awsVpcSubnet is a VPC and a subnet inside it. The subnet's VpcId is not known until the VPC
// exists, so the dependency is implicit: inside one query graph it is a β edge, and here it travels
// through the ledger because each effect is wrapped in its own durable writes.
type awsVpcSubnet struct{ blueprintBase }

func (awsVpcSubnet) Handle() string { return "aws-vpc-subnet" }

func (awsVpcSubnet) Summary() string {
	return "AWS: a VPC and a subnet in it. CIDRs are immutable, so a change is refused rather than replaced; tags are stamped at create and not converged."
}

func (awsVpcSubnet) Params() []Param {
	text := ParamType{Name: "string", Kind: "string"}
	cidr := ParamType{Name: "string", Format: "cidr", Kind: "string"}
	tags := ParamType{Name: "object", Kind: "object"}
	return []Param{
		{Name: "region", Type: text, Required: true, Description: "AWS region"},
		{Name: "vpc_cidr", Type: cidr, Required: true, Description: "VPC CIDR block, e.g. 10.0.0.0/16"},
		{Name: "subnet_cidr", Type: cidr, Required: true, Description: "subnet CIDR block, e.g. 10.0.1.0/24"},
		{Name: "vpc_tags", Type: tags, Description: `tags for the VPC, e.g. {"Name":"demo"}`},
		{Name: "subnet_tags", Type: tags, Description: "tags for the subnet"},
	}
}

func (b awsVpcSubnet) Resources(inputs map[string]string) ([]ManagedResource, error) {
	if err := check(b.Params(), inputs); err != nil {
		return nil, err
	}
	vpcTags, err := tagsOf(inputs["vpc_tags"])
	if err != nil {
		return nil, err
	}
	subnetTags, err := tagsOf(inputs["subnet_tags"])
	if err != nil {
		return nil, err
	}
	const vpc = "aws/ec2/vpc"
	return []ManagedResource{
		NewResource(vpc, "CreateVpc", cidrDoc(inputs["vpc_cidr"]), tagParams(vpcTags), nil),
		NewResource("aws/ec2/subnet", "CreateSubnet", cidrDoc(inputs["subnet_cidr"]), tagParams(subnetTags),
			map[string]string{"VpcId": vpc}),
	}, nil
}

func cidrDoc(cidr string) []byte { return fmt.Appendf(nil, `{"CidrBlock":%q}`, cidr) }

// tagParams marks each tag so the provider stamps it on the object rather than sending it as an
// ordinary request parameter.
func tagParams(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		out[awsec2.TagParamPrefix+k] = v
	}
	return out
}
