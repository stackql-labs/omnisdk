package sdk

import (
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/auth"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
	ep "github.com/stackql-labs/omnisdk/internal/system_g/endpoint"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// PrincipalSchema is the uniform access-review output: one row per principal, meaning the same thing
// on every cloud. An access review's whole value is a COMPLETE, comparable population — three
// provider-shaped answers are not a population, they are three lists.
var PrincipalSchema = []BlobColumn{
	{Name: "provider", Type: "string"},
	{Name: "principal_type", Type: "string"},           // user | group | service_principal
	{Name: "principal", Type: "string"},                // the human-readable identity
	{Name: "principal_id", Type: "string"},             // the provider's stable id
	{Name: "scope", Type: "string", Nullable: true},    // account / tenant / org it lives in
	{Name: "enabled", Type: "boolean", Nullable: true}, // null where the provider does not say
	{Name: "grant", Type: "string", Nullable: true},    // the role/policy held, where the source states one
	{Name: "created", Type: "string", Nullable: true},  // age is the finding for stale identities
}

var principalCols = principalColNames()

func principalColNames() []string {
	out := make([]string, len(PrincipalSchema))
	for i, c := range PrincipalSchema {
		out[i] = c.Name
	}
	return out
}

// principalEgress tags the provider and projects to the uniform schema, null for anything a provider
// does not surface — so every row is the same shape whatever produced it.
func principalEgress(provider string) []facade.Transform {
	return []facade.Transform{
		transform.NewConst(map[string]any{"provider": provider}),
		principalNormalize{},
		bind.NewSelect(principalCols),
	}
}

type principalNormalize struct{}

func (principalNormalize) Apply(in facade.Page) (facade.Record, error) {
	row := map[string]any{}
	if doc, ok := in.Doc(facade.AnonymousPayload); ok {
		if m, ok := doc.(map[string]any); ok {
			for k, v := range m {
				row[k] = v
			}
		}
	}
	// A provider that does not report a field reports null, never a fabricated default: "we do not
	// know" and "false" are different findings in an access review.
	for _, c := range PrincipalSchema {
		if _, ok := row[c.Name]; !ok {
			row[c.Name] = nil
		}
	}
	if v, ok := row["enabled"].(string); ok {
		row["enabled"] = v == "true"
	}
	return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(row)}), nil
}

// AWSIAMPrincipalsPlan lists IAM users. IAM is GLOBAL — one endpoint, no region in the URL — but
// SigV4 still signs into a region, which is why region is required and never inferred.
func AWSIAMPrincipalsPlan(region string, creds Credentials, endpoint string) plan.Plan {
	req := httpx.Request{
		Method: "POST",
		URL:    resolve(endpoint, ep.AWSIAM, nil) + "/",
		Body: httpx.Body{Encoding: httpx.EncodingForm, Params: map[string]any{
			"Action": "ListUsers", "Version": "2010-05-08",
		}},
		Continuation: httpx.Continuation{
			Kind:          httpx.ContPaginate,
			NextTokenPath: "ListUsersResponse.ListUsersResult.Marker",
			TokenParam:    "Marker",
		},
	}
	spec := plan.NewExchangeSpec("Users", nil, []string{"principal"},
		func(map[string]any) facade.Operator {
			send := httpx.Make(req, nil, NewSigV4Transform(NewSigV4Signer(region, "iam", creds, false)))(nil)
			checked := exchange.NewTransformExchange(0, send, httpx.NewRequireOK(), 1)
			decoded := exchange.NewTransformExchange(0, checked, transform.NewXMLToAgnostic(), 1)
			projected := exchange.NewTransformExchange(0, decoded,
				transform.NewProjection("ListUsersResponse.ListUsersResult.Users.member", []transform.Column{
					{Out: "principal", Path: "UserName"},
					{Out: "principal_id", Path: "UserId"},
					{Out: "created", Path: "CreateDate"},
					{Out: "arn", Path: "Arn"},
				}), 1)
			return exchange.NewExplodeRows(projected, 1)
		}, nil)
	return plan.NewPlan([]plan.ExchangeSpec{spec}, nil, nil,
		map[string]any{"principal_type": "user"}, principalEgress("aws"), encoder.NewJSONLEncoder())
}

// EntraPrincipalsPlan lists Entra ID users via Microsoft Graph. The directory is where the humans
// are; cloud IAM only holds what they were granted. Auth is the same client-credentials exchange the
// ARM audit uses — only the SCOPE differs, and that is derived from the service.
func EntraPrincipalsPlan(endpoint string, cfg auth.AuthStruct) (plan.Plan, error) {
	m, err := auth.New(cfg)
	if err != nil {
		return nil, err
	}
	users := httpx.Request{
		Method:  "GET",
		URL:     resolve(endpoint, ep.AzureGraph, nil) + "/v1.0/users",
		Headers: map[string]string{"Authorization": "Bearer {token}"},
		Query: map[string]string{
			"$select": "id,userPrincipalName,displayName,accountEnabled,createdDateTime",
		},
		// Graph pages by an absolute next link, not a token.
		Continuation: httpx.Continuation{Kind: httpx.ContFollow, NextTokenPath: "@odata.nextLink"},
	}
	usersSpec := plan.NewExchangeSpec("Users", []string{"token"}, []string{"principal"},
		func(bound map[string]any) facade.Operator {
			send := httpx.Make(users, nil)(bound)
			checked := exchange.NewTransformExchange(0, send, httpx.NewRequireOK(), 1)
			decoded := exchange.NewTransformExchange(0, checked, transform.NewJSONToAgnostic(), 1)
			projected := exchange.NewTransformExchange(0, decoded,
				transform.NewProjection("value", []transform.Column{
					{Out: "principal", Path: "userPrincipalName"},
					{Out: "principal_id", Path: "id"},
					{Out: "enabled", Path: "accountEnabled"},
					{Out: "created", Path: "createdDateTime"},
				}), 1)
			return exchange.NewExplodeRows(projected, 1)
		}, bind.NewInnerFlatten())

	specs := []plan.ExchangeSpec{usersSpec}
	betas := []plan.BetaEdge{}
	inputs := map[string]any{"principal_type": "user"}
	if m.NeedsTokenExchange() {
		if m.Kind() != auth.KindClientCredentials {
			return nil, errUnsupportedEntraAuth(m.Kind())
		}
		tr, _ := auth.AsOAuth(m)
		authSpec := plan.NewExchangeSpec("Auth", nil, []string{"token"},
			httpx.MakeAgnostic(auth.ClientCredentialsRequest(tr)),
			httpx.NewJSONExtract(map[string]string{"token": "access_token"}))
		specs = append([]plan.ExchangeSpec{authSpec}, specs...)
		betas = append(betas, plan.NewBetaEdge("Auth", "Users", "token", "token"))
	} else {
		tok, ok := auth.BearerToken(m)
		if !ok {
			return nil, errUnsupportedEntraAuth(m.Kind())
		}
		inputs["token"] = tok
	}
	return plan.NewPlan(specs, betas, nil, inputs, principalEgress("entra"), encoder.NewJSONLEncoder()), nil
}

// errUnsupportedEntraAuth names the two shapes Graph accepts, rather than failing vaguely.
func errUnsupportedEntraAuth(kind auth.Kind) error {
	return fmt.Errorf("entra auth: %s not supported here (use client_credentials or bearer)", kind)
}

// gcpIAMBindingsSpec reads a project's IAM policy and emits one row per MEMBER per role. GCP grants
// are policy bindings, not principal objects — the member string ("user:a@b.com", "serviceAccount:…")
// is both the identity and its type, so the principal population comes from the grants themselves.
func gcpIAMBindingsSpec(endpoint string) plan.ExchangeSpec {
	req := httpx.Request{
		Method:  "POST",
		URL:     gcpCRMv3Base(endpoint) + "/projects/{project}:getIamPolicy",
		Headers: map[string]string{"Authorization": "Bearer {token}"},
		Body:    httpx.Body{Encoding: httpx.EncodingJSON, Params: map[string]any{}},
	}
	return plan.NewExchangeSpec("IAMPolicy", []string{"token", "project"}, []string{"principal"},
		func(bound map[string]any) facade.Operator {
			send := httpx.Make(req, nil)(bound)
			checked := exchange.NewTransformExchange(0, send, httpx.NewRequireOK(), 1)
			decoded := exchange.NewTransformExchange(0, checked, transform.NewJSONToAgnostic(), 1)
			bindings := exchange.NewTransformExchange(0, decoded, gcpBindingRows{}, 1)
			return exchange.NewExplodeRows(bindings, 1)
		}, bind.NewInnerFlatten()) // a project with no readable policy contributes no principals
}

// gcpBindingRows flattens bindings[{role, members[]}] into one row per member. A projection cannot do
// it: the repeating element is nested inside another repeating element, and the role has to travel
// down with each member or the grant loses its meaning.
type gcpBindingRows struct{}

func (gcpBindingRows) Apply(in facade.Page) (facade.Record, error) {
	doc, _ := in.Doc(facade.AnonymousPayload)
	m, _ := doc.(map[string]any)
	bindings, _ := m["bindings"].([]any)
	out := make([]any, 0, len(bindings))
	for _, b := range bindings {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		role, _ := bm["role"].(string)
		members, _ := bm["members"].([]any)
		for _, mem := range members {
			s, ok := mem.(string)
			if !ok {
				continue
			}
			// "serviceAccount:x@y" — the prefix is the principal TYPE, which GCP states nowhere else.
			kind, name, found := strings.Cut(s, ":")
			if !found {
				kind, name = "unknown", s
			}
			out = append(out, map[string]any{
				"principal":      name,
				"principal_id":   s,
				"principal_type": gcpPrincipalType(kind),
				"grant":          role,
			})
		}
	}
	return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(out)}), nil
}

// gcpPrincipalType maps GCP's member prefixes onto the shared vocabulary.
func gcpPrincipalType(prefix string) string {
	switch prefix {
	case "user":
		return "user"
	case "group":
		return "group"
	case "serviceAccount":
		return "service_principal"
	default:
		return prefix
	}
}

// GCPIAMPrincipalsPlan lists principals granted a role on a project, or on EVERY project under an org
// — the same recursive descent the bucket audit uses, with IAM as the visitor.
func GCPIAMPrincipalsPlan(endpoint string, creds GCPCredentials, project, org string) plan.Plan {
	oauth, jwt := gcpOAuth(endpoint, creds, gcpCloudPlatformScope)
	if org != "" {
		specs := append([]plan.ExchangeSpec{oauth}, gcpOrgProjectSpecs(endpoint)...)
		specs = append(specs, gcpIAMBindingsSpec(endpoint))
		betas := []plan.BetaEdge{
			plan.NewBetaEdge("OAuth", "Folders", "token", "token"),
			plan.NewBetaEdge("OAuth", "Projects", "token", "token"),
			plan.NewBetaEdge("OAuth", "IAMPolicy", "token", "token"),
			plan.NewBetaEdge("Folders", "Projects", "node", "node"),
			plan.NewBetaEdge("Projects", "IAMPolicy", "project", "project"),
		}
		return plan.NewPlan(specs, betas, nil,
			map[string]any{"assertion": jwt, "org": org}, principalEgress("gcp"), encoder.NewJSONLEncoder())
	}
	specs := []plan.ExchangeSpec{oauth, gcpIAMBindingsSpec(endpoint)}
	betas := []plan.BetaEdge{plan.NewBetaEdge("OAuth", "IAMPolicy", "token", "token")}
	return plan.NewPlan(specs, betas, nil,
		map[string]any{"assertion": jwt, "project": project}, principalEgress("gcp"), encoder.NewJSONLEncoder())
}
