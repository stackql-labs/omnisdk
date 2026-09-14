package sdk

import (
	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"
)

// AWSEC2Call builds a single-exchange plan for one EC2 Query-API action: one wire call, one set of
// form parameters, one extraction.
//
// AWSProvisionPlan composes CreateVpc and CreateSubnet into one graph joined by a β edge. This is
// the same machinery taken apart, because an IaC run interleaves its own durable writes — intent
// before the call, identity after — around each effect individually. The dependency does not
// vanish; it moves from a β edge inside the plan to a binding through the ledger, where the
// subnet's VpcId comes from the vpc entry's recorded identity.
//
// extract maps an output name to a path in the XML response.
func AWSEC2Call(region string, creds Credentials, endpoint, action string, params, extract map[string]string) plan.Plan {
	form := map[string]any{"Action": action, "Version": ec2Version}
	for k, v := range params {
		form[k] = v
	}
	req := httpx.Request{
		Method: "POST",
		URL:    ec2Base(endpoint, region),
		Body:   httpx.Body{Encoding: httpx.EncodingForm, Params: form},
	}
	out := make([]string, 0, len(extract))
	for k := range extract {
		out = append(out, k)
	}
	spec := plan.NewExchangeSpec(action, nil, out,
		httpx.MakeAgnostic(req, NewSigV4Transform(ec2Signer(region, creds))),
		httpx.NewStatusBranch(map[string]facade.Transform{
			"200": httpx.NewExtract(transform.NewXMLToAgnostic(), extract, nil),
		}, nil))
	return plan.NewPlan([]plan.ExchangeSpec{spec}, nil, nil, nil, nil, encoder.NewJSONLEncoder())
}
