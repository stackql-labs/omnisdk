package sdk

import (
	"io"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
	ep "github.com/stackql-labs/omnisdk/internal/system_g/endpoint"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"
)

const (
	azureScope        = "https://management.azure.com/.default"
	azureSubsAPI      = "2020-01-01"
	azureNetworkAPI   = "2023-09-01"
	azureLoginDefault = "https://login.microsoftonline.com"
	azureMgmtDefault  = "https://management.azure.com"
)

func azureLoginBase(endpoint string) string {
	return resolve(endpoint, ep.AzureLogin, nil)
}

func azureMgmtBase(endpoint string) string {
	return resolve(endpoint, ep.AzureMgmt, nil)
}

// jsonListInner is the inner for a paginated JSON list API: send req (a bearer GET with its own
// continuation), decode JSON, project rowPath[] to cols, explode into one record per item. Shared
// by Azure (value[], follow nextLink) and GCP (items[], paginate pageToken).
func jsonListInner(req httpx.Request, rowPath string, cols []transform.Column) func(map[string]any) facade.Operator {
	if req.Headers == nil {
		req.Headers = map[string]string{"Authorization": "Bearer {token}"}
	}
	return func(bound map[string]any) facade.Operator {
		send := httpx.Make(req, nil)(bound)
		checked := exchange.NewTransformExchange(0, send, httpx.NewRequireOK(), 1) // non-2xx fails loudly, not silently empty
		decoded := exchange.NewTransformExchange(0, checked, transform.NewJSONToAgnostic(), 1)
		projected := exchange.NewTransformExchange(0, decoded, transform.NewProjection(rowPath, cols), 1)
		return exchange.NewExplodeRows(projected, 1)
	}
}

// azureList lists an Azure ARM collection (value[], nextLink pagination).
func azureList(urlTemplate string, cols []transform.Column) func(map[string]any) facade.Operator {
	return jsonListInner(httpx.Request{
		Method:       "GET",
		URL:          urlTemplate,
		Continuation: httpx.Continuation{Kind: httpx.ContFollow, NextTokenPath: "nextLink"},
	}, "value", cols)
}

// NewAzureVNetSubnets enumerates every subnet under every virtual network under every subscription
// in the tenant. Token → Subscriptions → VNets → Subnets, each level fanning out in parallel over
// its parent (subscriptions across the org, VNets per subscription, subnets per VNet). All httpx.
// azureTokenReq is the client-credentials token request (tenant/client_id/client_secret are κ).
func azureTokenReq(login string) httpx.Request {
	return httpx.Request{
		Method: "POST", URL: login + "/{tenant}/oauth2/v2.0/token",
		Body: httpx.Body{Encoding: httpx.EncodingForm, Params: map[string]any{
			"grant_type":    "client_credentials",
			"client_id":     "{client_id}",
			"client_secret": "{client_secret}",
			"scope":         azureScope,
		}},
	}
}

func NewAzureVNetSubnets(id int64, endpoint, tenant, clientID, clientSecret string, w io.Writer) facade.Operator {
	return plan.Compose(id, AzureVNetSubnetsPlan(endpoint, tenant, clientID, clientSecret), w)
}

// AzureVNetSubnetsPlan is NewAzureVNetSubnets as an uncomposed plan.Plan (see S3ListPlan for why).
func AzureVNetSubnetsPlan(endpoint, tenant, clientID, clientSecret string) plan.Plan {
	mgmt := azureMgmtBase(endpoint)

	specs := []plan.ExchangeSpec{
		plan.NewExchangeSpec("Token", []string{"tenant", "client_id", "client_secret"}, []string{"token"},
			httpx.MakeAgnostic(azureTokenReq(azureLoginBase(endpoint))), httpx.NewJSONExtract(map[string]string{"token": "access_token"})),
		plan.NewExchangeSpec("Subscriptions", []string{"token"}, []string{"subscription_id"},
			azureList(mgmt+"/subscriptions?api-version="+azureSubsAPI,
				[]transform.Column{{Out: "subscription_id", Path: "subscriptionId"}}), nil),
		plan.NewExchangeSpec("VNets", []string{"token", "subscription_id"}, []string{"vnet_id", "vnet_name", "location"},
			azureList(mgmt+"/subscriptions/{subscription_id}/providers/Microsoft.Network/virtualNetworks?api-version="+azureNetworkAPI,
				[]transform.Column{
					{Out: "vnet_id", Path: "id"},
					{Out: "vnet_name", Path: "name"},
					{Out: "location", Path: "location"},
				}), nil),
		plan.NewExchangeSpec("Subnets", []string{"token", "vnet_id"}, []string{"subnet_id", "subnet_name", "address_prefix"},
			azureList(mgmt+"{vnet_id}/subnets?api-version="+azureNetworkAPI,
				[]transform.Column{
					{Out: "subnet_id", Path: "id"},
					{Out: "subnet_name", Path: "name"},
					{Out: "address_prefix", Path: "properties.addressPrefix"},
				}), nil),
	}
	betas := []plan.BetaEdge{
		plan.NewBetaEdge("Token", "Subscriptions", "token", "token"),
		plan.NewBetaEdge("Token", "VNets", "token", "token"),
		plan.NewBetaEdge("Token", "Subnets", "token", "token"),
		plan.NewBetaEdge("Subscriptions", "VNets", "subscription_id", "subscription_id"),
		plan.NewBetaEdge("VNets", "Subnets", "vnet_id", "vnet_id"),
	}
	inputs := map[string]any{"tenant": tenant, "client_id": clientID, "client_secret": clientSecret}
	egress := []facade.Transform{bind.NewSelect([]string{
		"subscription_id", "vnet_name", "vnet_id", "subnet_name", "subnet_id", "address_prefix",
	})}
	return plan.NewPlan(specs, betas, nil, inputs, egress, encoder.NewJSONLEncoder())
}
