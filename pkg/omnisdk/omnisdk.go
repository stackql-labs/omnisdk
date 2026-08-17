// Package omnisdk is the public facade for consumers such as stackql. A resource is addressed by a
// PATH; a consumer can list resources (optionally regex-filtered), get one's metadata (required
// params + response schema), then run it — New(path, args) returns a Plan whose Open streams rows.
// The system_g engine stays internal; this package exposes only its own DTOs.
//
// Today the catalog is hand-authored from what we know about each case. The future state infers the
// same metadata (params, schema, auth) from provider documents — behind this exact interface, so
// consumers do not change. Transport (REST vs gRPC) is an internal detail, never surfaced here.
package omnisdk

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/stackql-labs/omnisdk/internal/system_g/abort"
	"github.com/stackql-labs/omnisdk/internal/system_g/admit"
	"github.com/stackql-labs/omnisdk/internal/system_g/auth"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/endpoint"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/sdk"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/grpcx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/retry"
	"github.com/stackql-labs/omnisdk/internal/system_g/schedule"
	"github.com/stackql-labs/omnisdk/internal/system_g/secret"
	"github.com/stackql-labs/omnisdk/internal/system_g/sink"
	"github.com/stackql-labs/omnisdk/internal/system_g/trace"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

// Auth is the caller-supplied authentication config (a pared, stackql-shaped auth struct). It is the
// ONE place credentials flow in: every field resolves inline value → named env var → file, and each
// env-var/file NAME defaults to the provider's canonical variable when omitted — so a nil Auth audits
// with the standard environment, while a consumer (e.g. stackql) can inject secrets per-request by
// setting the inline values. The parenthesised names are the canonical defaults.
type Auth struct {
	// Type selects the method for token/header auth (bearer, client_credentials); ignored for AWS/GCP,
	// whose method is implied by the resource.
	Type string `json:"type,omitempty"`

	// AWS SigV4 (aws.*).
	AccessKeyID           string `json:"access_key_id,omitempty"`
	SecretAccessKey       string `json:"secret_access_key,omitempty"`
	SessionToken          string `json:"session_token,omitempty"`
	AccessKeyIDEnvVar     string `json:"access_key_id_env_var,omitempty"`     // AWS_ACCESS_KEY_ID
	SecretAccessKeyEnvVar string `json:"secret_access_key_env_var,omitempty"` // AWS_SECRET_ACCESS_KEY
	SessionTokenEnvVar    string `json:"session_token_env_var,omitempty"`     // AWS_SESSION_TOKEN

	// Credential blob (GCP service-account JSON; bearer / api-key token): inline → env → file.
	Credentials         string `json:"credentials,omitempty"`
	CredentialsEnvVar   string `json:"credentialsenvvar,omitempty"`   // GCP: GOOGLE_CREDENTIALS
	CredentialsFilePath string `json:"credentialsfilepath,omitempty"` // GCP: $GOOGLE_APPLICATION_CREDENTIALS

	// Header injection (bearer, api_key).
	ValuePrefix string `json:"valuePrefix,omitempty"`
	Name        string `json:"name,omitempty"`

	// OAuth2 / Azure client-credentials.
	Scopes             []string `json:"scopes,omitempty"`
	TokenURL           string   `json:"token_url,omitempty"`
	Tenant             string   `json:"tenant,omitempty"`
	TenantEnvVar       string   `json:"tenant_env_var,omitempty"` // AZURE_TENANT_ID
	ClientID           string   `json:"client_id,omitempty"`
	ClientSecret       string   `json:"client_secret,omitempty"`
	ClientIDEnvVar     string   `json:"client_id_env_var,omitempty"`     // AZURE_CLIENT_ID
	ClientSecretEnvVar string   `json:"client_secret_env_var,omitempty"` // AZURE_CLIENT_SECRET
}

func (a Auth) internal() auth.AuthStruct {
	return auth.AuthStruct{
		Type: a.Type, Credentials: a.Credentials, CredentialsEnvVar: a.CredentialsEnvVar,
		CredentialsFilePath: a.CredentialsFilePath, ValuePrefix: a.ValuePrefix, Name: a.Name,
		Scopes: a.Scopes, TokenURL: a.TokenURL, ClientID: a.ClientID, ClientSecret: a.ClientSecret,
		ClientIDEnvVar: a.ClientIDEnvVar, ClientSecretEnvVar: a.ClientSecretEnvVar,
	}
}

// Param describes one input a resource accepts (scope: project, org, region, …). Required params are
// enforced by New — never inferred.
type Param struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

// Resource is a "thing" (e.g. buckets), addressed by a dot-path like "google.storage.buckets". It is
// a collection of methods and takes no input itself; it carries a canonical Schema (a detail
// representation of the thing, JSON Schema draft 2020-12) purely for discovery.
type Resource struct {
	Path    string         `json:"path"`
	Summary string         `json:"summary"`
	Schema  map[string]any `json:"schema"`
}

// Method is an operation on a resource (accessor / mutator), addressed by "<resource>.<method>" —
// e.g. "google.storage.buckets.list". Its signature is Params (input) + Schema (output JSON Schema).
// Methods are discovered per-resource.
type Method struct {
	Path     string  `json:"path"`
	Resource string  `json:"resource"`
	Summary  string  `json:"summary"`
	Params   []Param `json:"params"`
	// ExactlyOne names groups of params of which EXACTLY ONE must be supplied — mutually exclusive
	// alternatives that `Required` cannot express (a GCP audit is scoped by project OR by org, never
	// both, never neither). Declared here rather than checked inside a plan builder so the constraint
	// is part of the published signature: discoverable, enforced in one place for every method, and
	// stated in each method's OWN param names — a composite that renames an input reports its name,
	// not its leg's.
	ExactlyOne [][]string     `json:"exactly_one,omitempty"`
	Schema     map[string]any `json:"schema"`
}

// Args are the inputs to run a resource: explicit scope Params, optional Auth (config-driven-auth
// resources), an endpoint override, run Tuning, and an optional traffic log.
type Args struct {
	Params   map[string]string
	Auth     *Auth
	Endpoint string
	Tuning   Tuning
	Log      io.Writer
}

func (a Args) param(name string) string { return a.Params[name] }

// UnmarshalJSON lets Endpoint be written either way a consumer would naturally write it: a URL
// string, or the per-service object inline. Both land in the same string field the engine resolves,
// so nothing downstream branches — but a caller never has to embed escaped JSON inside JSON.
//
//	"endpoint": "http://127.0.0.1:8085"
//	"endpoint": {"aws.s3": {"host": "127.0.0.1", "port": "8085", "scheme": "http"}}
func (a *Args) UnmarshalJSON(data []byte) error {
	type alias Args // shed this method, so the rest of the DTO decodes normally
	aux := struct {
		Endpoint json.RawMessage `json:"endpoint"`
		*alias
	}{alias: (*alias)(a)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(aux.Endpoint) == 0 {
		return nil
	}
	var asURL string
	if err := json.Unmarshal(aux.Endpoint, &asURL); err == nil {
		a.Endpoint = asURL
		return nil
	}
	a.Endpoint = string(aux.Endpoint) // the object form, verbatim — endpoint.Parse reads it
	return nil
}

// Tuning are the run knobs; a zero value uses sensible defaults.
type Tuning struct {
	Parallelism int
	MaxPerHost  int
	RetryTries  int
	RetryRate   float64
	Limit       int
	Timeout     time.Duration
}

// Row is one result record; Rows is a forward-only cursor over them (the caller owns its lifecycle).
type Row = map[string]any

type Rows interface {
	Next() bool
	Row() Row
	Err() error
	Close() error
}

// Plan is a runnable, planned query. Open executes it under ctx and streams rows.
type Plan interface {
	Open(ctx context.Context) (Rows, error)
}

// blobSchema is the JSON Schema (draft 2020-12) for the uniform, normalized blob-audit JSONL rows —
// the same across every provider. It is DERIVED from sdk.BlobSchema (the same columns the egress
// select emits), so the published contract can't drift from the actual output. Nullable columns are
// typed as a "<type>|null" union; every column is present in every row, so all are required.
var blobSchema = jsonSchema(sdk.BlobSchema)

// Schemas for the non-blob methods, each DERIVED from the columns that method's egress select emits
// (single source of truth → the contract can't drift). str is the string-column shorthand.
func str(name string) sdk.BlobColumn { return sdk.BlobColumn{Name: name, Type: "string"} }

// principalSchema is the uniform access-review row, DERIVED from sdk.PrincipalSchema so the published
// contract cannot drift from what the egress actually emits.
var principalSchema = jsonSchema(sdk.PrincipalSchema)

var (
	s3ListSchema    = jsonSchema([]sdk.BlobColumn{str("name"), str("created"), str("arn")})
	awsNetSchema    = jsonSchema([]sdk.BlobColumn{str("vpc_id"), str("vpc_description"), str("subnet_id"), str("subnet_description")})
	gcpNetSchema    = jsonSchema([]sdk.BlobColumn{str("vpc_id"), str("network_link"), str("subnet_id"), str("subnet_link")})
	subnetSchema    = jsonSchema([]sdk.BlobColumn{str("subscription_id"), str("vnet_name"), str("vnet_id"), str("subnet_name"), str("subnet_id"), str("address_prefix")})
	genericObjectSc = map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "additionalProperties": true}
)

// jsonSchema renders columns as a JSON Schema object. For lossless transfer a numeric column is
// carried as a string with Type "string": `format` names the logical type (the OpenAPI-style idiom,
// e.g. int64/double), and the extension keyword `x-omnisdk-lossless: true` states unambiguously that
// the string is an encoding to be decoded per `format`, not textual data.
func jsonSchema(cols []sdk.BlobColumn) map[string]any {
	props := map[string]any{}
	required := make([]string, 0, len(cols))
	for _, c := range cols {
		prop := map[string]any{}
		if c.Nullable {
			prop["type"] = []any{c.Type, "null"}
		} else {
			prop["type"] = c.Type
		}
		if c.Format != "" {
			prop["format"] = c.Format
			prop["x-omnisdk-lossless"] = true
		}
		props[c.Name] = prop
		required = append(required, c.Name)
	}
	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
		"required":             required,
	}
}

// withParams publishes a method's declared INPUTS as columns of its response schema, so the signature
// and the result contract are one thing: a consumer reads the schema and sees the scope that produced
// each row — which region/project/org a bucket came from — without holding onto the request. For the
// cross-cloud composite this is the only way to attribute a row to its scope at all. Derived from
// Params at read time, so it cannot drift from the signature. A required input is always present
// (plain type); an optional one is nullable and null when it was not supplied. x-omnisdk-input marks
// the column as echoed input rather than provider data. A provider column of the same name always
// wins — the response never loses data to an input echo. Rows carry these values (see echoParams).
func withParams(base map[string]any, params []Param) map[string]any {
	if len(params) == 0 {
		return base
	}
	out := make(map[string]any, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	props := map[string]any{}
	if p, ok := base["properties"].(map[string]any); ok {
		for k, v := range p {
			props[k] = v
		}
	}
	var required []string
	if r, ok := base["required"].([]string); ok {
		required = append(required, r...)
	}
	for _, p := range params {
		if _, clash := props[p.Name]; clash {
			continue // a provider column of this name already exists; data wins
		}
		var typ any = "string"
		if !p.Required {
			typ = []any{"string", "null"}
		}
		props[p.Name] = map[string]any{
			"type": typ, "description": p.Description, "x-omnisdk-input": true,
		}
		required = append(required, p.Name)
	}
	out["properties"], out["required"] = props, required
	// The echo adds keys the base schema never declared, so a closed base must reopen to stay truthful.
	if _, ok := base["properties"]; !ok {
		out["additionalProperties"] = true
	}
	return out
}

// echoParams are a method's declared inputs as they were actually supplied, for stamping onto every
// row. nil when the method takes none — then rows are untouched. An absent optional input is null,
// matching the nullable column withParams publishes for it.
func echoParams(params []Param, args Args) map[string]any {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]any, len(params))
	for _, p := range params {
		if v := args.param(p.Name); v != "" {
			out[p.Name] = v
		} else {
			out[p.Name] = nil
		}
	}
	return out
}

// memberArgs is what a composite hands its legs: ONLY its own declared params. The published
// signature is therefore authoritative — an undeclared param cannot slip through to a leg and quietly
// steer the query while the row's echoed scope columns say it was never supplied. Auth, endpoint and
// tuning are untouched; they are not part of the signature.
func memberArgs(args Args, def methodDef) Args {
	params := make(map[string]string, len(def.Params))
	for _, p := range def.Params {
		if v := args.param(p.Name); v != "" {
			params[p.Name] = v
		}
	}
	out := args
	out.Params = params
	return out
}

// unionParams is the deduplicated signature of several methods — what a merged run echoes.
func unionParams(methodPaths []string) []Param {
	var out []Param
	seen := map[string]bool{}
	for _, path := range methodPaths {
		for _, p := range methods[path].Params {
			if !seen[p.Name] {
				seen[p.Name] = true
				out = append(out, p)
			}
		}
	}
	return out
}

type methodDef struct {
	Method
	// build plans this method's single query graph. Set on a leaf method; nil on a composite.
	build func(args Args) (plan.Plan, error)
	// members, when non-empty, makes this a COMPOSITE method: its plan is the FOREST of these member
	// methods' plans, merged into ONE cursor. A composite is defined BY REFERENCE to its legs, so it
	// cannot drift from them — adding a provider is one entry here, not a new hand-rolled plan.
	members []string
}

// resources is the hand-authored resource catalog (the "things", with a canonical schema each).
var resources = map[string]Resource{
	"aws.s3.buckets": {
		Path:    "aws.s3.buckets",
		Summary: "AWS S3 buckets",
		Schema:  blobSchema,
	},
	"google.storage.buckets": {
		Path:    "google.storage.buckets",
		Summary: "GCP Cloud Storage buckets",
		Schema:  blobSchema,
	},
	"azure.storage.containers": {
		Path:    "azure.storage.containers",
		Summary: "Azure blob containers (the bucket analogue: subscription → account → container)",
		Schema:  blobSchema,
	},
	"aws.iam.principals": {
		Path:    "aws.iam.principals",
		Summary: "AWS IAM principals (users)",
		Schema:  principalSchema,
	},
	"gcp.iam.principals": {
		Path:    "gcp.iam.principals",
		Summary: "GCP principals granted a role on a project or across an org",
		Schema:  principalSchema,
	},
	"entra.identities": {
		Path:    "entra.identities",
		Summary: "Entra ID directory identities",
		Schema:  principalSchema,
	},
	// The access-review population: every principal, every cloud, one comparable shape. The legs are
	// hand-authored today and doc-compiled later — the address, schema and composite do not change
	// when they are, which is the point of resolving them behind a catalog entry.
	"omni.iam.principals": {
		Path:    "omni.iam.principals",
		Summary: "Principals across every identity source (AWS IAM + Entra ID)",
		Schema:  principalSchema,
	},
	// The cross-cloud composite as a FIRST-CLASS thing: blob stores wherever they live. Every provider
	// leg already normalizes to the one blob schema, so the union is a single coherent resource — not a
	// client-side stitch-up — and it carries that same canonical schema.
	"omni.storage.buckets": {
		Path:    "omni.storage.buckets",
		Summary: "Blob stores across every cloud (AWS S3 + Azure storage accounts + GCP buckets)",
		Schema:  blobSchema,
	},
	"aws.ec2.networks": {
		Path:    "aws.ec2.networks",
		Summary: "AWS EC2 VPC networks",
		Schema:  awsNetSchema,
	},
	"gcp.compute.networks": {
		Path:    "gcp.compute.networks",
		Summary: "GCP Compute VPC networks",
		Schema:  gcpNetSchema,
	},
	"azure.network.subnets": {
		Path:    "azure.network.subnets",
		Summary: "Azure virtual-network subnets",
		Schema:  subnetSchema,
	},
}

// methods is the hand-authored method catalog, keyed by dot-path "<resource>.<method>". Later, both
// maps are generated from provider documents behind the same shape.
var methods = map[string]methodDef{
	"aws.s3.buckets.list": {
		Method: Method{
			Path:     "aws.s3.buckets.list",
			Resource: "aws.s3.buckets",
			Summary:  "List S3 buckets (encryption/public/versioning) in the credentials' account",
			Params:   []Param{{Name: "region", Required: true, Description: "AWS region"}},
			Schema:   blobSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			creds, err := awsCreds(args)
			if err != nil {
				return nil, err
			}
			return sdk.AWSBlobPlan(args.param("region"), creds, args.Endpoint), nil
		},
	},
	"azure.storage.containers.list": {
		Method: Method{
			Path:     "azure.storage.containers.list",
			Resource: "azure.storage.containers",
			Summary:  "List blob containers (encryption/public/https/logging) across every subscription the principal can read",
			Params:   nil, // scope is the SP's reach; auth carries the identity
			Schema:   blobSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			a, err := azureAuth(args, endpoint.AzureMgmt)
			if err != nil {
				return nil, err
			}
			return sdk.AzureBlobAuthPlan(args.Endpoint, a)
		},
	},
	"google.storage.buckets.list": {
		Method: Method{
			Path:     "google.storage.buckets.list",
			Resource: "google.storage.buckets",
			Summary:  "List buckets (encryption/public/versioning) for a project OR a whole org",
			Params: []Param{
				{Name: "project", Required: false, Description: "single GCP project (mutually exclusive with org)"},
				{Name: "org", Required: false, Description: "audit the whole org, recursive folder→project descent (mutually exclusive with project)"},
				{Name: "grpc_target", Required: false, Description: "run the bucket audit over the gRPC Storage API at this host:port instead of REST (transport only; identical rows)"},
				{Name: "grpc_plaintext", Required: false, Description: "\"true\" to dial grpc_target without TLS (for a local mock); default TLS"},
			},
			ExactlyOne: [][]string{{"google_project", "google_org"}}, // one project or a whole org, never both
			Schema:     blobSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			project, org := args.param("google_project"), args.param("google_org")
			creds, err := gcpCreds(args)
			if err != nil {
				return nil, err
			}
			// Transport is a hidden detail chosen by config: a grpc_target routes the SAME audit over the
			// dynamic gRPC Storage API (proto-only serde); otherwise REST. The rows are identical either way.
			if target := args.param("grpc_target"); target != "" {
				d, err := grpcx.Load()
				if err != nil {
					return nil, err
				}
				opts := grpcDialOpts(args.param("grpc_plaintext") == "true")
				if org != "" {
					return sdk.GCPBlobOrgGRPCPlan(d, args.Endpoint, target, creds, org, opts...), nil
				}
				return sdk.GCPBlobGRPCPlan(d, args.Endpoint, target, creds, project, opts...), nil
			}
			if org != "" {
				return sdk.GCPBlobOrgPlan(args.Endpoint, creds, org), nil
			}
			return sdk.GCPBlobPlan(args.Endpoint, creds, project), nil
		},
	},
	// The cross-cloud bucket composite, named and discoverable as ONE method: its plan is the forest of
	// the three per-cloud legs fanned into a single cursor — the same shape NewMerged produces, but the
	// consumer selects one path instead of assembling (and having to know) the member list. Its Params
	// are the union of the legs' scope inputs; Azure contributes none (its scope is the principal's
	// reach). Each leg still validates its own params when built, so this signature cannot drift.
	"omni.storage.buckets.list": {
		Method: Method{
			Path:     "omni.storage.buckets.list",
			Resource: "omni.storage.buckets",
			Summary:  "List blob stores (encryption/public/versioning) across AWS+Azure+GCP as one uniform result set",
			Params: []Param{
				{Name: "region", Required: true, Description: "AWS region (AWS leg)"},
				{Name: "google_project", Required: false, Description: "single GCP project (mutually exclusive with google_org)"},
				{Name: "google_org", Required: false, Description: "audit the whole GCP org, recursive folder→project descent (mutually exclusive with google_project)"},
			},
			ExactlyOne: [][]string{{"google_project", "google_org"}},
			Schema:     blobSchema,
		},
		members: []string{
			"aws.s3.buckets.list",
			"azure.storage.containers.list",
			"google.storage.buckets.list",
		},
	},
	"aws.iam.principals.list": {
		Method: Method{
			Path:     "aws.iam.principals.list",
			Resource: "aws.iam.principals",
			Summary:  "List IAM users in the credentials' account",
			Params:   []Param{{Name: "region", Required: true, Description: "AWS region to sign with (IAM is global; SigV4 still scopes to a region)"}},
			Schema:   principalSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			creds, err := awsCreds(args)
			if err != nil {
				return nil, err
			}
			return sdk.AWSIAMPrincipalsPlan(args.param("region"), creds, args.Endpoint), nil
		},
	},
	"entra.identities.list": {
		Method: Method{
			Path:     "entra.identities.list",
			Resource: "entra.identities",
			Summary:  "List Entra ID users (Microsoft Graph); scope is the tenant the credentials belong to",
			Params:   nil, // the tenant is the credentials' own; nothing to choose
			Schema:   principalSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			// Same client-credentials exchange as the ARM audit — only the SCOPE differs, and that is
			// derived from the service rather than configured.
			a, err := azureAuth(args, endpoint.AzureGraph)
			if err != nil {
				return nil, err
			}
			return sdk.EntraPrincipalsPlan(args.Endpoint, a)
		},
	},
	"gcp.iam.principals.list": {
		Method: Method{
			Path:     "gcp.iam.principals.list",
			Resource: "gcp.iam.principals",
			Summary:  "List principals holding a role on a project OR across a whole org",
			Params: []Param{
				{Name: "google_project", Required: false, Description: "single GCP project (mutually exclusive with google_org)"},
				{Name: "google_org", Required: false, Description: "every project under the org, recursive folder descent (mutually exclusive with google_project)"},
			},
			ExactlyOne: [][]string{{"google_project", "google_org"}},
			Schema:     principalSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			creds, err := gcpCreds(args)
			if err != nil {
				return nil, err
			}
			return sdk.GCPIAMPrincipalsPlan(args.Endpoint, creds, args.param("google_project"), args.param("google_org")), nil
		},
	},
	"omni.iam.principals.list": {
		Method: Method{
			Path:     "omni.iam.principals.list",
			Resource: "omni.iam.principals",
			Summary:  "Every principal across every identity source, as one comparable population",
			Params: []Param{
				{Name: "region", Required: true, Description: "AWS region to sign with (AWS leg)"},
				{Name: "google_project", Required: false, Description: "single GCP project (mutually exclusive with google_org)"},
				{Name: "google_org", Required: false, Description: "every project under the GCP org (mutually exclusive with google_project)"},
			},
			ExactlyOne: [][]string{{"google_project", "google_org"}},
			Schema:     principalSchema,
		},
		members: []string{"aws.iam.principals.list", "entra.identities.list", "gcp.iam.principals.list"},
	},
	"aws.s3.buckets.enumerate": {
		Method: Method{
			Path:     "aws.s3.buckets.enumerate",
			Resource: "aws.s3.buckets",
			Summary:  "Enumerate S3 buckets (name/created/arn) in the credentials' account",
			Params:   []Param{{Name: "region", Required: true, Description: "AWS region"}},
			Schema:   s3ListSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			creds, err := awsCreds(args)
			if err != nil {
				return nil, err
			}
			return sdk.S3ListPlan(1, args.param("region"), creds, args.Endpoint), nil
		},
	},
	"aws.s3.buckets.encryption": {
		Method: Method{
			Path:     "aws.s3.buckets.encryption",
			Resource: "aws.s3.buckets",
			Summary:  "List buckets ⋈ per-bucket GetBucketEncryption (the β bowtie)",
			Params:   []Param{{Name: "region", Required: true, Description: "AWS region"}},
			Schema:   genericObjectSc,
		},
		build: func(args Args) (plan.Plan, error) {
			creds, err := awsCreds(args)
			if err != nil {
				return nil, err
			}
			return sdk.S3EncryptionPlan(1, args.param("region"), creds, args.Endpoint), nil
		},
	},
	"aws.ec2.networks.provision": {
		Method: Method{
			Path:     "aws.ec2.networks.provision",
			Resource: "aws.ec2.networks",
			Summary:  "Create a VPC then a subnet in it (β flow); CREATES REAL AWS RESOURCES",
			Params: []Param{
				{Name: "region", Required: true, Description: "AWS region"},
				{Name: "vpc_cidr", Required: true, Description: "VPC CIDR block, e.g. 10.0.0.0/16"},
				{Name: "subnet_cidr", Required: true, Description: "subnet CIDR block, e.g. 10.0.1.0/24"},
			},
			Schema: awsNetSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			creds, err := awsCreds(args)
			if err != nil {
				return nil, err
			}
			return sdk.AWSProvisionPlan(1, args.param("region"), creds, args.Endpoint, args.param("vpc_cidr"), args.param("subnet_cidr")), nil
		},
	},
	"gcp.compute.networks.provision": {
		Method: Method{
			Path:     "gcp.compute.networks.provision",
			Resource: "gcp.compute.networks",
			Summary:  "OAuth → CreateNetwork → poll → CreateSubnet → poll (β token + async); CREATES REAL GCP RESOURCES",
			Params: []Param{
				{Name: "project", Required: true, Description: "GCP project to provision into"},
				{Name: "region", Required: false, Description: "GCP region for the subnetwork (default us-central1)"},
			},
			Schema: gcpNetSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			creds, err := gcpCreds(args)
			if err != nil {
				return nil, err
			}
			region := args.param("region")
			if region == "" {
				region = "us-central1"
			}
			return sdk.GCPProvisionPlan(1, region, creds, args.Endpoint, args.param("project")), nil
		},
	},
	"azure.network.subnets.list": {
		Method: Method{
			Path:     "azure.network.subnets.list",
			Resource: "azure.network.subnets",
			Summary:  "Every subnet under every VNet under every subscription the SP can read",
			Params:   nil, // scope is the SP's reach; identity from AZURE_* env
			Schema:   subnetSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			tenant, clientID, clientSecret, err := azureNativeCreds(args)
			if err != nil {
				return nil, err
			}
			return sdk.AzureVNetSubnetsPlan(args.Endpoint, tenant, clientID, clientSecret), nil
		},
	},
}

// Catalog is the discovery + planning seam: list resources (regex-filtered), list a resource's
// methods, get a resource or method's metadata, and plan a method for execution. The built-in
// catalog is hand-authored today; a document-inferred catalog implements this same interface later,
// so consumers (and the CLI) never change.
type Catalog interface {
	Resources(filter string) ([]Resource, error)
	Methods(resource string) ([]Method, error)
	GetResource(path string) (Resource, bool)
	GetMethod(path string) (Method, bool)
	New(method string, args Args) (Plan, error)
	// NewMerged plans several methods and fans their rows into ONE cursor — System-G owns even
	// disjoint query graphs under a single output node, which the consumer opens as one Plan and
	// cannot tell apart from a single graph (see plan.MergeRows). This is the multi-cloud audit.
	NewMerged(methods []string, args Args) (Plan, error)
}

// Default returns the built-in, hand-authored catalog.
func Default() Catalog { return builtin{} }

type builtin struct{}

func (builtin) Resources(filter string) ([]Resource, error) {
	re, err := regexp.Compile(filter)
	if err != nil {
		return nil, fmt.Errorf("omnisdk: bad filter %q: %w", filter, err)
	}
	var out []Resource
	for _, r := range resources {
		if re.MatchString(r.Path) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (builtin) Methods(resource string) ([]Method, error) {
	if _, ok := resources[resource]; !ok {
		return nil, fmt.Errorf("omnisdk: unknown resource %q", resource)
	}
	var out []Method
	for _, m := range methods {
		if m.Resource == resource {
			out = append(out, m.published())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// published is the method as a consumer sees it: the response schema carries the method's declared
// inputs as columns (withParams), so signature and result contract are read as one.
func (d methodDef) published() Method {
	m := d.Method
	m.Schema = withParams(m.Schema, m.Params)
	return m
}

func (builtin) GetResource(path string) (Resource, bool) {
	r, ok := resources[path]
	return r, ok
}

func (builtin) GetMethod(path string) (Method, bool) {
	m, ok := methods[path]
	if !ok {
		return Method{}, false
	}
	return m.published(), true
}

func (builtin) New(method string, args Args) (Plan, error) {
	if err := checkEndpoint(args); err != nil {
		return nil, err
	}
	plans, err := buildPlans(method, args, map[string]bool{})
	if err != nil {
		return nil, err
	}
	echo := echoParams(methods[method].Params, args)
	// A leaf is one graph, a composite a forest — both open as one Plan, so a consumer selecting
	// omni.storage.buckets.list cannot tell it apart from a single-provider method.
	if len(plans) == 1 {
		return &cannedPlan{plan: plans[0], args: args, echo: echo}, nil
	}
	return &mergedPlan{plans: plans, args: args, echo: echo}, nil
}

func (builtin) NewMerged(methodPaths []string, args Args) (Plan, error) {
	if len(methodPaths) == 0 {
		return nil, fmt.Errorf("omnisdk: NewMerged needs at least one method")
	}
	if err := checkEndpoint(args); err != nil {
		return nil, err
	}
	plans := make([]plan.Plan, 0, len(methodPaths))
	for _, m := range methodPaths {
		pls, err := buildPlans(m, args, map[string]bool{})
		if err != nil {
			return nil, err
		}
		plans = append(plans, pls...)
	}
	return &mergedPlan{plans: plans, args: args, echo: echoParams(unionParams(methodPaths), args)}, nil
}

// buildPlans looks up a method, enforces its required params (never inferred), and builds its plan(s).
// A leaf method yields exactly one plan; a COMPOSITE yields its members' plans — the forest the caller
// presents as one cursor. Recursion lets a composite name composites; seen breaks a catalog cycle
// rather than overflowing the stack (the catalog is hand-authored today, generated later).
func buildPlans(method string, args Args, seen map[string]bool) ([]plan.Plan, error) {
	def, ok := methods[method]
	if !ok {
		return nil, fmt.Errorf("omnisdk: unknown method %q", method)
	}
	if seen[method] {
		return nil, fmt.Errorf("omnisdk: composite method cycle at %q", method)
	}
	for _, p := range def.Params {
		if p.Required && args.param(p.Name) == "" {
			return nil, fmt.Errorf("omnisdk: method %q requires param %q", method, p.Name)
		}
	}
	for _, group := range def.ExactlyOne {
		var supplied []string
		for _, name := range group {
			if args.param(name) != "" {
				supplied = append(supplied, name)
			}
		}
		if len(supplied) != 1 {
			return nil, fmt.Errorf("omnisdk: method %q requires exactly one of params %s (got %d)",
				method, quoteAll(group), len(supplied))
		}
	}
	if len(def.members) == 0 {
		pl, err := def.build(args)
		if err != nil {
			return nil, err
		}
		return []plan.Plan{pl}, nil
	}
	seen[method] = true
	defer delete(seen, method)
	inner := memberArgs(args, def)
	out := make([]plan.Plan, 0, len(def.members))
	for _, m := range def.members {
		pls, err := buildPlans(m, inner, seen)
		if err != nil {
			return nil, fmt.Errorf("omnisdk: method %q: %w", method, err)
		}
		out = append(out, pls...)
	}
	return out, nil
}

// Package-level convenience over the Default catalog (consumers may inject their own instead).
func Resources(filter string) ([]Resource, error) { return Default().Resources(filter) }
func Methods(resource string) ([]Method, error)   { return Default().Methods(resource) }
func GetResource(path string) (Resource, bool)    { return Default().GetResource(path) }
func GetMethod(path string) (Method, bool)        { return Default().GetMethod(path) }
func New(method string, args Args) (Plan, error)  { return Default().New(method, args) }
func NewMerged(methods []string, args Args) (Plan, error) {
	return Default().NewMerged(methods, args)
}

// checkEndpoint rejects a malformed Endpoint spec HERE, where the error can name the caller. Request
// construction resolves the same spec far too deep to fail usefully, so it degrades to the real clouds
// — which is exactly the silent-wrong-target this catches. Endpoint is either empty (the real clouds),
// a base URL (every service at that one host), or a JSON object of service → base URL for pointing one
// provider at a mock while another stays real.
func checkEndpoint(args Args) error {
	if _, err := endpoint.Parse(args.Endpoint); err != nil {
		return fmt.Errorf("omnisdk: %w", err)
	}
	return nil
}

// NewFromDoc plans a resource's SELECT straight from a provider DOCUMENT, with no catalog entry: the
// document supplies the call, and the declared auth scheme is applied implicitly. This is the same
// Plan a catalog method returns, so a consumer runs it identically — the difference is only where the
// metadata came from, which is the whole point of the document path.
func NewFromDoc(doc []byte, resource string, args Args) (Plan, error) {
	if err := checkEndpoint(args); err != nil {
		return nil, err
	}
	reg, err := dsl.NewRegistry(gotemplate.Evaluators()...)
	if err != nil {
		return nil, err
	}
	pl, err := docx.SelectPlan(doc, resource, docInputs(args), reg, docOptions(args)...)
	if err != nil {
		return nil, err
	}
	return &cannedPlan{plan: pl, args: args}, nil
}

// DocCatalog lists a provider BUNDLE (a directory holding provider.yaml and its services/): the
// services whose documents are present, and every addressable exchange. An address is
// "<provider>.<service>.<resource>". The provider lists more services than any bundle ships, and only
// the present ones are addressable.
func DocCatalog(dir string) (services []string, addresses []string, err error) {
	c, err := stackqldoc.Open(os.DirFS(dir))
	if err != nil {
		return nil, nil, err
	}
	return c.Services(), c.Paths(), nil
}

// DocResources lists a service's resources — every one it declares, not only those with a runnable
// SELECT.
func DocResources(dir, service string) ([]string, error) {
	c, err := stackqldoc.Open(os.DirFS(dir))
	if err != nil {
		return nil, err
	}
	return c.Resources(service)
}

// DocMethod is a method as a document declares it: its name, the SQL verb it is bound to (empty when
// none), and the operation behind it.
type DocMethod struct {
	Name        string `json:"name"`
	SQLVerb     string `json:"sql_verb,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

// DocMethods lists a resource's methods.
func DocMethods(dir, service, resource string) ([]DocMethod, error) {
	c, err := stackqldoc.Open(os.DirFS(dir))
	if err != nil {
		return nil, err
	}
	ms, err := c.Methods(service, resource)
	if err != nil {
		return nil, err
	}
	out := make([]DocMethod, len(ms))
	for i, m := range ms {
		out[i] = DocMethod{Name: m.Name(), SQLVerb: m.SQLVerb(), OperationID: m.OperationID()}
	}
	return out, nil
}

// NewFromCatalog plans one addressed exchange out of a provider bundle. Same Plan a catalog method
// returns, so a consumer runs it identically.
func NewFromCatalog(dir, address string, args Args) (Plan, error) {
	if err := checkEndpoint(args); err != nil {
		return nil, err
	}
	c, err := stackqldoc.Open(os.DirFS(dir))
	if err != nil {
		return nil, err
	}
	ex, err := c.Exchange(address)
	if err != nil {
		return nil, err
	}
	reg, err := dsl.NewRegistry(gotemplate.Evaluators()...)
	if err != nil {
		return nil, err
	}
	pl, err := docx.PlanFor(ex, docInputs(args), reg, docOptions(args)...)
	if err != nil {
		return nil, err
	}
	return &cannedPlan{plan: pl, args: args}, nil
}

// docInputs are the caller's params as κ inputs.
func docInputs(args Args) map[string]any {
	out := map[string]any{}
	for k, v := range args.Params {
		out[k] = v
	}
	return out
}

// docOptions carry the endpoint override and AWS credentials, resolved exactly as for a catalog
// method — a document that declares signing but finds no credentials fails at plan time.
func docOptions(args Args) []docx.Option {
	var opts []docx.Option
	if args.Endpoint != "" {
		opts = append(opts, docx.WithBaseURL(args.Endpoint))
	}
	if creds, err := awsCreds(args); err == nil {
		opts = append(opts, docx.WithAWSCredentials(creds))
	}
	return opts
}

// authOf returns args.Auth, or a zero Auth when none was supplied — so resolution always reads from a
// struct and every field falls back to its canonical env var / file.
func authOf(args Args) Auth {
	if args.Auth != nil {
		return *args.Auth
	}
	return Auth{}
}

// orStr returns v, or fallback when v is empty — defaults an env-var/file NAME to the canonical one.
func orStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// awsCreds resolves AWS SigV4 credentials from the Auth DTO: inline value, else the named env var
// (defaulted to the canonical AWS_* var). SessionToken is optional (STS/assumed-role).
func awsCreds(args Args) (sdk.Credentials, error) {
	a := authOf(args)
	id, err := secret.Require("AWS access key id",
		secret.Literal(a.AccessKeyID), secret.Env(orStr(a.AccessKeyIDEnvVar, "AWS_ACCESS_KEY_ID"))).Resolve()
	if err != nil {
		return sdk.Credentials{}, err
	}
	key, err := secret.Require("AWS secret access key",
		secret.Literal(a.SecretAccessKey), secret.Env(orStr(a.SecretAccessKeyEnvVar, "AWS_SECRET_ACCESS_KEY"))).Resolve()
	if err != nil {
		return sdk.Credentials{}, err
	}
	tok := secret.Optional(secret.Literal(a.SessionToken), secret.Env(orStr(a.SessionTokenEnvVar, "AWS_SESSION_TOKEN")))
	return sdk.Credentials{AccessKeyID: id, SecretAccessKey: key, SessionToken: tok}, nil
}

// azureAuth builds the Azure storage-account config-driven auth from the Auth DTO. An explicit bearer
// passes through; otherwise it is a client_credentials exchange — id/secret inline or from AZURE_*
// env, the token URL taken as-is or built from the tenant (endpoint-aware, so a mock's
// /<tenant>/oauth2/v2.0/token is reachable). Everything defaults to canonical AZURE_* vars, so a nil
// Auth still audits with the standard environment.
func azureAuth(args Args, service string) (auth.AuthStruct, error) {
	a := authOf(args)
	if a.Type == "bearer" {
		return a.internal(), nil
	}
	cfg := a.internal()
	cfg.Type = "client_credentials"
	cfg.ClientIDEnvVar = orStr(cfg.ClientIDEnvVar, "AZURE_CLIENT_ID")
	cfg.ClientSecretEnvVar = orStr(cfg.ClientSecretEnvVar, "AZURE_CLIENT_SECRET")
	// Resolve id/secret HERE, requiring them. The exchange treats them as OPTIONAL, so an unresolved
	// credential would otherwise POST an empty client_id, get no access_token back, and bind an empty
	// {token} into every ARM request — surfacing as an opaque provider 401 ("the 'Authorization' header
	// is missing the access token") instead of naming the credential that is actually absent.
	id, err := secret.Require("Azure client id",
		secret.Literal(cfg.ClientID), secret.Env(cfg.ClientIDEnvVar)).Resolve()
	if err != nil {
		return auth.AuthStruct{}, err
	}
	sec, err := secret.Require("Azure client secret",
		secret.Literal(cfg.ClientSecret), secret.Env(cfg.ClientSecretEnvVar)).Resolve()
	if err != nil {
		return auth.AuthStruct{}, err
	}
	cfg.ClientID, cfg.ClientSecret = id, sec
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{azureScope(service)}
	}
	if cfg.TokenURL == "" {
		tenant, err := secret.Require("Azure tenant id",
			secret.Literal(a.Tenant), secret.Env(orStr(a.TenantEnvVar, "AZURE_TENANT_ID"))).Resolve()
		if err != nil {
			return auth.AuthStruct{}, err
		}
		cfg.TokenURL = azureTokenURL(args.Endpoint, tenant)
	}
	return cfg, nil
}

// azureScope is the OAuth resource scope for an Azure service. It is DERIVED, not configured: an
// Azure scope IS the service's own base URL plus "/.default", so the registry already knows it. That
// matters because one run legitimately needs several — ARM for role assignments, Graph for the
// directory behind them — and a single constant cannot express two.
//
// Deliberately the REGISTERED default, not the resolved endpoint: a mock changes where the request
// goes, never which resource the token is for. Signing a token for http://127.0.0.1 would be asking
// Entra for an audience that does not exist.
func azureScope(service string) string {
	base := endpoint.Default(service)
	if base == "" {
		base = endpoint.Default(endpoint.AzureMgmt)
	}
	return strings.TrimRight(base, "/") + "/.default"
}

// azureTokenURL is the OAuth2 token endpoint for a tenant, targeting an endpoint override when set.
func azureTokenURL(spec, tenant string) string {
	// Resolve through the SAME service resolver the engine uses, not by treating the spec as a URL.
	// This is the one host assembled up here (client_credentials needs the tenant spliced in), so it is
	// also the one that can silently disagree with the engine about where Azure lives.
	base := endpoint.Default(endpoint.AzureLogin)
	if ep, err := endpoint.Parse(spec); err == nil {
		base = ep.Resolve(endpoint.AzureLogin, nil)
	}
	return base + "/" + tenant + "/oauth2/v2.0/token"
}

// azureNativeCreds resolves the SP tenant/client/secret from the Auth DTO (inline → canonical AZURE_*
// env) for methods that run their OWN token exchange (native κ inputs, e.g. the VNet/subnet audit).
func azureNativeCreds(args Args) (tenant, clientID, clientSecret string, err error) {
	a := authOf(args)
	if tenant, err = secret.Require("Azure tenant id",
		secret.Literal(a.Tenant), secret.Env(orStr(a.TenantEnvVar, "AZURE_TENANT_ID"))).Resolve(); err != nil {
		return
	}
	if clientID, err = secret.Require("Azure client id",
		secret.Literal(a.ClientID), secret.Env(orStr(a.ClientIDEnvVar, "AZURE_CLIENT_ID"))).Resolve(); err != nil {
		return
	}
	clientSecret, err = secret.Require("Azure client secret",
		secret.Literal(a.ClientSecret), secret.Env(orStr(a.ClientSecretEnvVar, "AZURE_CLIENT_SECRET"))).Resolve()
	return
}

// grpcDialOpts selects the transport security for a gRPC audit: plaintext (a local mock) or TLS.
func grpcDialOpts(plaintext bool) []grpc.DialOption {
	if plaintext {
		return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}
	return []grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{}))}
}

// gcpCreds resolves the GCP service-account key from the Auth DTO: inline JSON, else the named env var
// (default GOOGLE_CREDENTIALS), else the file (default $GOOGLE_APPLICATION_CREDENTIALS).
func gcpCreds(args Args) (sdk.GCPCredentials, error) {
	a := authOf(args)
	raw, err := secret.Require("GCP credentials",
		secret.Literal(a.Credentials),
		secret.Env(orStr(a.CredentialsEnvVar, "GOOGLE_CREDENTIALS")),
		secret.File(orStr(a.CredentialsFilePath, os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")))).Resolve()
	if err != nil {
		return sdk.GCPCredentials{}, err
	}
	return sdk.ParseGCPCredentials([]byte(raw))
}

type cannedPlan struct {
	plan plan.Plan
	args Args
	echo map[string]any // the method's declared inputs, stamped onto every row (see withParams)
}

func (c *cannedPlan) Open(parent context.Context) (Rows, error) {
	runCtx, cancel := c.decorate(parent)
	op := plan.ComposeRows(1, c.plan)
	// The engine runs on runCtx (cancelled by abort/limit/timeout to stop PRODUCERS). The cursor is
	// read on the caller's ctx, so already-buffered rows are drained even after an internal abort —
	// abort means "stop producing", not "discard produced rows".
	return &rows{recs: op.Open(runCtx), read: parent, cancel: cancel, echo: c.echo}, nil
}

// decorate carries the run policies (retry, admission, fan-out, abort, result budget, trace) and an
// optional timeout onto ctx; the returned cancel releases them on Close.
func (c *cannedPlan) decorate(parent context.Context) (context.Context, context.CancelFunc) {
	ctx := parent
	timeoutCancel := func() {}
	if c.args.Tuning.Timeout > 0 {
		ctx, timeoutCancel = context.WithTimeout(parent, c.args.Tuning.Timeout)
	}
	ctx, abortCancel := abort.WithSignal(ctx)
	ctx = abort.WithLimit(ctx, c.args.Tuning.Limit)
	logw, closeLog := logSink(c.args.Log)
	ctx = trace.WithWriter(ctx, logw)
	ctx = retry.WithPolicy(ctx, retry.New(retry.Config{
		Tries:    orInt(c.args.Tuning.RetryTries, 4),
		Backoff:  retry.NewDecorrelatedJitter(250*time.Millisecond, 30*time.Second),
		Governor: retry.NewRateGovernor(orFloat(c.args.Tuning.RetryRate, 20), 5),
	}))
	ctx = admit.WithAdmissions(ctx, admit.PerScope(orInt(c.args.Tuning.MaxPerHost, 8)))
	ctx = schedule.WithLimiter(ctx, admit.NewSemaphore(orInt(c.args.Tuning.Parallelism, 16)))
	return ctx, func() { abortCancel(); timeoutCancel(); closeLog() }
}

// logSink wraps the caller's trace writer in a concurrency-safe async sink — every exchange's log tap
// writes to it concurrently, so the facade owns this (consumers pass a plain io.Writer). nil or
// io.Discard means logging is off. The returned close flushes the queue on Close.
func logSink(w io.Writer) (io.Writer, func()) {
	if w == nil || w == io.Discard {
		return io.Discard, func() {}
	}
	aw := sink.NewAsync(w, 256)
	return aw, func() { _ = aw.Close() }
}

// mergedPlan is N method plans presented as one row cursor (see Catalog.NewMerged). All legs share
// ONE decorated ctx — so the run policies, and crucially the --limit result budget, apply globally
// across the union, not per-leg.
type mergedPlan struct {
	plans []plan.Plan
	args  Args
	echo  map[string]any // the union signature's inputs, stamped onto every row (see withParams)
}

func (m *mergedPlan) Open(parent context.Context) (Rows, error) {
	c := &cannedPlan{args: m.args}
	runCtx, cancel := c.decorate(parent)
	// Engine on runCtx (abort/limit stop PRODUCERS); cursor read on parent so a limit-abort drains the
	// already-produced rows rather than discarding them (mirrors cannedPlan.Open).
	op := plan.MergeComposeRows(1, m.plans...)
	return &rows{recs: op.Open(runCtx), read: parent, cancel: cancel, echo: m.echo}, nil
}

type rows struct {
	recs   facade.Records
	read   context.Context
	cancel context.CancelFunc
	echo   map[string]any
}

func (r *rows) Next() bool { return r.recs.Next(r.read) }

func (r *rows) Row() Row {
	m, _ := bind.DocMap(r.recs.Record())
	if len(r.echo) == 0 {
		return m
	}
	// Copy rather than mutate what the engine handed back, and let provider data win any name clash —
	// mirrors the schema withParams publishes.
	out := make(Row, len(m)+len(r.echo))
	for k, v := range r.echo {
		out[k] = v
	}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Err reports a genuine failure; an intentional stop (abort / limit / Close) surfaces as
// context.Canceled, which is not an error.
func (r *rows) Err() error {
	if err := r.recs.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func (r *rows) Close() error {
	r.cancel()
	return r.recs.Close()
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orFloat(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

// quoteAll renders param names for an error: 'a', 'b', 'c'.
func quoteAll(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "'" + n + "'"
	}
	return strings.Join(out, ", ")
}
