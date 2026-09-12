// Package awsec2 implements facade.Effector over EC2's Query API.
//
// It is the seam between the durable machinery and System-G: every call here builds an ordinary
// single-exchange plan and drains it, so an IaC effect and a query travel the same path.
//
// EC2 is the awkward case on purpose. The merge produces a JSON document, the wire takes form
// parameters, and identity is minted by the service — so this is where the per-provider mapping
// that cannot be derived from a schema has to live.
package awsec2

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/sdk"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
)

// CorrelationTag is the tag key stamped on every object this package creates. It is what makes the
// ledger a cache rather than the sole link to reality: lose the store and the objects are still
// found by enumerating the scope. Terraform cannot do this, which is why its state file feels so
// precious.
const CorrelationTag = "omnisdk:key"

// TagParamPrefix marks a parameter as a tag to stamp rather than a form parameter to send.
//
// Tags travel as parameters rather than as fields of the desired document because DescribeVpcs is
// not extracting the tag set yet: a tag inside the mutation would compare against an actual that
// never carries it, and every re-apply would read as drifted. So v1 stamps tags at create and does
// not converge them.
const TagParamPrefix = "tag:"

// action is one exchange's wire mapping: the EC2 action, what to pull out of the response, and
// which extracted field is the object's identity.
type action struct {
	name string
	// extract maps an output name to a path in the XML response.
	extract map[string]string
	// identity names the extracted field that addresses the object afterwards. Empty where the
	// call mints nothing — a delete.
	identity string
	// addressedBy names the form parameter that carries an existing object's identity, so a delete
	// or a read can be built from the ledger entry alone.
	addressedBy string
	// tagged says the create stamps the correlation key, and the read can filter on it.
	tagged bool
	// resourceType is the EC2 tag specification's resource type, needed to stamp at create.
	resourceType string
	// idField is the response path to the object's own id, used when rediscovering by tag.
	idField string
}

// actions is the per-provider metadata: not derivable by differencing schemas, and data rather
// than code per resource.
var actions = map[string]action{
	"CreateVpc": {
		name:         "CreateVpc",
		extract:      map[string]string{"vpc_id": "CreateVpcResponse.vpc.vpcId"},
		identity:     "vpc_id",
		tagged:       true,
		resourceType: "vpc",
	},
	"CreateSubnet": {
		name:         "CreateSubnet",
		extract:      map[string]string{"subnet_id": "CreateSubnetResponse.subnet.subnetId"},
		identity:     "subnet_id",
		tagged:       true,
		resourceType: "subnet",
	},
	"CreateSecurityGroup": {
		name:         "CreateSecurityGroup",
		extract:      map[string]string{"group_id": "CreateSecurityGroupResponse.groupId"},
		identity:     "group_id",
		tagged:       true,
		resourceType: "security-group",
	},
	"DeleteSecurityGroup": {
		name:        "DeleteSecurityGroup",
		extract:     map[string]string{"deleted": "DeleteSecurityGroupResponse.return"},
		addressedBy: "GroupId",
	},
	"DescribeSecurityGroups": {
		name: "DescribeSecurityGroups",
		extract: map[string]string{
			"GroupName":        "DescribeSecurityGroupsResponse.securityGroupInfo.item.groupName",
			"GroupDescription": "DescribeSecurityGroupsResponse.securityGroupInfo.item.groupDescription",
			"id":               "DescribeSecurityGroupsResponse.securityGroupInfo.item.groupId",
		},
		addressedBy: "GroupId.1",
		tagged:      true,
		idField:     "id",
	},
	"DeleteVpc": {
		name:        "DeleteVpc",
		extract:     map[string]string{"deleted": "DeleteVpcResponse.return"},
		addressedBy: "VpcId",
	},
	"DeleteSubnet": {
		name:        "DeleteSubnet",
		extract:     map[string]string{"deleted": "DeleteSubnetResponse.return"},
		addressedBy: "SubnetId",
	},
	"DescribeVpcs": {
		name: "DescribeVpcs",
		extract: map[string]string{
			"CidrBlock": "DescribeVpcsResponse.vpcSet.item.cidrBlock",
			"id":        "DescribeVpcsResponse.vpcSet.item.vpcId",
		},
		addressedBy: "VpcId.1",
		tagged:      true,
		idField:     "id",
	},
	"DescribeSubnets": {
		name: "DescribeSubnets",
		extract: map[string]string{
			"CidrBlock": "DescribeSubnetsResponse.subnetSet.item.cidrBlock",
			"id":        "DescribeSubnetsResponse.subnetSet.item.subnetId",
		},
		addressedBy: "SubnetId.1",
		tagged:      true,
		idField:     "id",
	},
}

// reads maps a mutating exchange to the exchange that reads the same object. Convergence needs
// live reads; without one the log degrades from fact to belief.
var reads = map[string]string{
	"CreateVpc":           "DescribeVpcs",
	"CreateSubnet":        "DescribeSubnets",
	"CreateSecurityGroup": "DescribeSecurityGroups",
}

type effector struct {
	region   string
	creds    sdk.Credentials
	endpoint string
}

// New returns an Effector for one region. endpoint overrides the real AWS host, which is what lets
// a run be exercised against a stand-in.
func New(region string, creds sdk.Credentials, endpoint string) facade.Effector {
	return &effector{region: region, creds: creds, endpoint: endpoint}
}

func (e *effector) Effect(ctx context.Context, exchange string, k facade.LedgerKey, in facade.EffectInput) ([]byte, error) {
	a, ok := actions[exchange]
	if !ok {
		return nil, fmt.Errorf("awsec2: no action for exchange %q", exchange)
	}
	params, err := e.params(a, k, in)
	if err != nil {
		return nil, err
	}
	row, err := e.call(ctx, a, params)
	if err != nil {
		return nil, fmt.Errorf("awsec2: %s %s: %w", a.name, k, err)
	}
	if a.identity == "" {
		return in.Identity, nil
	}
	id, ok := row[a.identity]
	if !ok || id == "" {
		return nil, fmt.Errorf("awsec2: %s %s returned no %s", a.name, k, a.identity)
	}
	return []byte(id), nil
}

func (e *effector) Read(ctx context.Context, exchange string, k facade.LedgerKey, in facade.EffectInput) ([]byte, []byte, bool, error) {
	readExchange, ok := reads[exchange]
	if !ok {
		// No usable read for this exchange: convergence weakens to idempotence, and the caller is
		// told rather than handed an empty document that would read as "absent".
		return nil, nil, false, nil
	}
	a := actions[readExchange]

	// A read is addressed, never mutated, so its parameters are built from scratch. The caller's
	// params belong to the create — a subnet's bound VpcId is a CreateSubnet parameter, and
	// DescribeSubnets rejects it outright.
	params := map[string]string{}
	switch {
	case len(in.Identity) > 0:
		params[a.addressedBy] = string(in.Identity)
	case a.tagged:
		// Identity unknown — this run has no record of the object. Rediscover it by the correlation
		// key rather than assuming absence, so a lost or empty ledger converges instead of creating
		// a duplicate.
		params["Filter.1.Name"] = "tag:" + CorrelationTag
		params["Filter.1.Value.1"] = string(k)
	default:
		return nil, nil, false, nil
	}
	row, err := e.call(ctx, a, params)
	if err != nil {
		return nil, nil, false, fmt.Errorf("awsec2: %s %s: %w", a.name, k, err)
	}
	if len(row) == 0 {
		return nil, nil, true, nil
	}
	// The id is reported separately from actual so a caller holding no identity can adopt an object
	// it did not create in this run.
	identity := in.Identity
	if a.idField != "" && row[a.idField] != "" {
		identity = []byte(row[a.idField])
	}
	delete(row, a.idField)
	b, err := json.Marshal(row)
	if err != nil {
		return nil, nil, false, err
	}
	return b, identity, true, nil
}

// params builds the form parameters: the mutation's fields are EC2 parameter names, the caller's
// params address the object, and a recorded identity fills the parameter the action is addressed
// by. Explicit params win, since a binding is more specific than a derived default.
func (e *effector) params(a action, k facade.LedgerKey, in facade.EffectInput) (map[string]string, error) {
	params := map[string]string{}
	if len(in.Mutation) > 0 && a.identity != "" {
		var fields map[string]any
		if err := json.Unmarshal(in.Mutation, &fields); err != nil {
			return nil, fmt.Errorf("awsec2: mutation is not a JSON object: %w", err)
		}
		for f, v := range fields {
			if v == nil {
				// An unset has no expression in the Query API: a field is sent or it is not.
				continue
			}
			params[f] = fmt.Sprint(v)
		}
	}
	if a.addressedBy != "" && len(in.Identity) > 0 {
		params[a.addressedBy] = string(in.Identity)
	}
	for name, v := range in.Params {
		if strings.HasPrefix(name, TagParamPrefix) {
			continue
		}
		params[name] = v
	}
	if a.tagged && a.resourceType != "" {
		// The correlation key is stamped on the object itself: the link back to the ledger key that
		// survives losing the ledger. Caller tags follow it, ordered so a request is reproducible.
		params["TagSpecification.1.ResourceType"] = a.resourceType
		params["TagSpecification.1.Tag.1.Key"] = CorrelationTag
		params["TagSpecification.1.Tag.1.Value"] = string(k)

		names := make([]string, 0, len(in.Params))
		for name := range in.Params {
			if strings.HasPrefix(name, TagParamPrefix) {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		for i, name := range names {
			n := strconv.Itoa(i + 2)
			params["TagSpecification.1.Tag."+n+".Key"] = strings.TrimPrefix(name, TagParamPrefix)
			params["TagSpecification.1.Tag."+n+".Value"] = in.Params[name]
		}
	}
	return params, nil
}

// call runs the single-exchange plan and returns the one row it yields, empty when the response
// carried nothing.
func (e *effector) call(ctx context.Context, a action, params map[string]string) (map[string]string, error) {
	p := sdk.AWSEC2Call(e.region, e.creds, e.endpoint, a.name, params, a.extract)
	recs := plan.ComposeRows(1, p).Open(ctx)
	defer recs.Close()

	row := map[string]string{}
	for recs.Next(ctx) {
		doc, ok := bind.DocMap(recs.Record())
		if !ok {
			continue
		}
		for out := range a.extract {
			if v, present := doc[out]; present && v != nil {
				row[out] = fmt.Sprint(v)
			}
		}
	}
	if err := recs.Err(); err != nil {
		return nil, err
	}
	return row, nil
}
