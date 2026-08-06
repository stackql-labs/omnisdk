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
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/sdk"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/grpcx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/retry"
	"github.com/stackql-labs/omnisdk/internal/system_g/schedule"
	"github.com/stackql-labs/omnisdk/internal/system_g/secret"
	"github.com/stackql-labs/omnisdk/internal/system_g/sink"
	"github.com/stackql-labs/omnisdk/internal/system_g/trace"
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
	Path     string         `json:"path"`
	Resource string         `json:"resource"`
	Summary  string         `json:"summary"`
	Params   []Param        `json:"params"`
	Schema   map[string]any `json:"schema"`
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

type methodDef struct {
	Method
	build func(args Args) (plan.Plan, error)
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
	"azure.storage.accounts": {
		Path:    "azure.storage.accounts",
		Summary: "Azure storage accounts",
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
	"azure.storage.accounts.list": {
		Method: Method{
			Path:     "azure.storage.accounts.list",
			Resource: "azure.storage.accounts",
			Summary:  "List storage accounts (encryption/public/https) across every subscription the principal can read",
			Params:   nil, // scope is the SP's reach; auth carries the identity
			Schema:   blobSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			a, err := azureAuth(args)
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
			Schema: blobSchema,
		},
		build: func(args Args) (plan.Plan, error) {
			project, org := args.param("project"), args.param("org")
			if (project == "") == (org == "") {
				return nil, fmt.Errorf("omnisdk: google.storage.buckets.list needs exactly one of param 'project' or 'org'")
			}
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
			out = append(out, m.Method)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (builtin) GetResource(path string) (Resource, bool) {
	r, ok := resources[path]
	return r, ok
}

func (builtin) GetMethod(path string) (Method, bool) {
	m, ok := methods[path]
	return m.Method, ok
}

func (builtin) New(method string, args Args) (Plan, error) {
	pl, err := buildPlan(method, args)
	if err != nil {
		return nil, err
	}
	return &cannedPlan{plan: pl, args: args}, nil
}

func (builtin) NewMerged(methodPaths []string, args Args) (Plan, error) {
	if len(methodPaths) == 0 {
		return nil, fmt.Errorf("omnisdk: NewMerged needs at least one method")
	}
	plans := make([]plan.Plan, 0, len(methodPaths))
	for _, m := range methodPaths {
		pl, err := buildPlan(m, args)
		if err != nil {
			return nil, err
		}
		plans = append(plans, pl)
	}
	return &mergedPlan{plans: plans, args: args}, nil
}

// buildPlan looks up a method, enforces its required params (never inferred), and builds its plan.
func buildPlan(method string, args Args) (plan.Plan, error) {
	def, ok := methods[method]
	if !ok {
		return nil, fmt.Errorf("omnisdk: unknown method %q", method)
	}
	for _, p := range def.Params {
		if p.Required && args.param(p.Name) == "" {
			return nil, fmt.Errorf("omnisdk: method %q requires param %q", method, p.Name)
		}
	}
	return def.build(args)
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
func azureAuth(args Args) (auth.AuthStruct, error) {
	a := authOf(args)
	if a.Type == "bearer" {
		return a.internal(), nil
	}
	cfg := a.internal()
	cfg.Type = "client_credentials"
	cfg.ClientIDEnvVar = orStr(cfg.ClientIDEnvVar, "AZURE_CLIENT_ID")
	cfg.ClientSecretEnvVar = orStr(cfg.ClientSecretEnvVar, "AZURE_CLIENT_SECRET")
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"https://management.azure.com/.default"}
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

// azureTokenURL is the OAuth2 token endpoint for a tenant, targeting an endpoint override when set.
func azureTokenURL(endpoint, tenant string) string {
	base := "https://login.microsoftonline.com"
	if endpoint != "" {
		base = strings.TrimRight(endpoint, "/")
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
}

func (c *cannedPlan) Open(parent context.Context) (Rows, error) {
	runCtx, cancel := c.decorate(parent)
	op := plan.ComposeRows(1, c.plan)
	// The engine runs on runCtx (cancelled by abort/limit/timeout to stop PRODUCERS). The cursor is
	// read on the caller's ctx, so already-buffered rows are drained even after an internal abort —
	// abort means "stop producing", not "discard produced rows".
	return &rows{recs: op.Open(runCtx), read: parent, cancel: cancel}, nil
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
}

func (m *mergedPlan) Open(parent context.Context) (Rows, error) {
	c := &cannedPlan{args: m.args}
	runCtx, cancel := c.decorate(parent)
	// Engine on runCtx (abort/limit stop PRODUCERS); cursor read on parent so a limit-abort drains the
	// already-produced rows rather than discarding them (mirrors cannedPlan.Open).
	op := plan.MergeComposeRows(1, m.plans...)
	return &rows{recs: op.Open(runCtx), read: parent, cancel: cancel}, nil
}

type rows struct {
	recs   facade.Records
	read   context.Context
	cancel context.CancelFunc
}

func (r *rows) Next() bool { return r.recs.Next(r.read) }

func (r *rows) Row() Row {
	m, _ := bind.DocMap(r.recs.Record())
	return m
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
