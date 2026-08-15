package sdk

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/admit"
	"github.com/stackql-labs/omnisdk/internal/system_g/auth"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/descent"
	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
	ep "github.com/stackql-labs/omnisdk/internal/system_g/endpoint"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/schedule"
	"github.com/stackql-labs/omnisdk/internal/system_g/termination"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// Shallow blob-store audit across AWS/Azure/GCP: per-object security posture as a uniform row —
// {provider, name, encryption_status, key_management, public, versioning, https}. Absent fields
// are null (a provider may not surface one shallowly). key_management normalizes the encryption
// key owner across clouds; the raw signals are kept alongside.

const (
	azureStorageAPI = "2023-01-01"
	gcpStorageScope = "https://www.googleapis.com/auth/devstorage.read_only"
	// org descent lists projects AND reads buckets, so it needs the broader read-only scope.
	gcpCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform.read-only"
)

// BlobColumn describes one column of the uniform blob-audit row: its name, JSON-Schema scalar type,
// whether it can be null (a provider may not surface an attribute shallowly), and an optional Format.
// This is the SINGLE source of truth — the projection/select and any published JSON Schema both
// derive from it, so they cannot drift.
//
// LOSSLESS TRANSFER: JSON numbers are lossy (int64/uint64/double lose precision through a float64
// parse), so a value that must round-trip exactly is carried as a STRING with Type "string" and a
// Format naming its true type (OpenAPI-style: "int64", "uint64", "double"). The client reads the
// string and reconstructs the number from Format. (proto3 JSON already string-encodes 64-bit ints,
// so the gRPC path is aligned.) No current column needs it; the mechanism is here for when they do.
type BlobColumn struct {
	Name     string
	Type     string // "string" | "boolean" | "integer" | "number"
	Nullable bool
	Format   string // JSON Schema format for lossless string-encoded numerics ("int64" | "double" | …)
}

// BlobSchema is the uniform audit output schema (order-significant for the select).
var BlobSchema = []BlobColumn{
	{Name: "provider", Type: "string"},
	{Name: "name", Type: "string"},
	{Name: "encryption_status", Type: "string", Nullable: true}, // null when no CMEK (provider-managed default)
	{Name: "encryption_class", Type: "string"},                  // always classified (provider-managed / customer-managed / unknown)
	{Name: "public", Type: "boolean", Nullable: true},           // null where not surfaced (e.g. Azure allowBlobPublicAccess unset)
	{Name: "versioning", Type: "boolean", Nullable: true},       // null where not surfaced shallowly (Azure)
	{Name: "https", Type: "boolean", Nullable: true},            // null where not surfaced shallowly (AWS)
	{Name: "access_logging", Type: "boolean", Nullable: true},   // is access/audit logging turned on
	{Name: "access_log_target", Type: "string", Nullable: true}, // where those logs land; null when off
}

// blobCols is the column-name list the egress select projects to (derived from BlobSchema).
var blobCols = blobColNames()

func blobColNames() []string {
	out := make([]string, len(BlobSchema))
	for i, c := range BlobSchema {
		out[i] = c.Name
	}
	return out
}

func gcpStorageBase(endpoint string) string {
	return resolve(endpoint, ep.GCPStorage, nil)
}

// gcpCRMv3Base is Cloud Resource Manager v3 (the org→folder→project hierarchy) — v3 addresses every
// node uniformly as "<type>/<id>" (organizations/…, folders/…), so one descent walks the whole tree.
func gcpCRMv3Base(endpoint string) string {
	return resolve(endpoint, ep.GCPCRM, nil)
}

// gcpOrgProjectSpecs is the reusable GCP org SCOPE SOURCE: a recursive folder descent (Folders, via
// descent.NewRecursive — the self-β cycle) followed by projects-per-node (Projects), yielding a
// {project, token} row for EVERY project under the org, folder-nested ones included. It is oblivious
// to what gets audited: append ANY visitor exchange that binds project+token — buckets today, VMs /
// networks / DBs tomorrow — and the descent is unchanged. Needs κ input "org" and β "token" (OAuth).
func gcpOrgProjectSpecs(endpoint string) []plan.ExchangeSpec {
	crm := gcpCRMv3Base(endpoint)
	foldersReq := httpx.Request{
		Method: "GET", URL: crm + "/folders",
		Query:        map[string]string{"parent": "{parent}"},
		Continuation: httpx.Continuation{Kind: httpx.ContPaginate, NextTokenPath: "nextPageToken", TokenParam: "pageToken"},
	}
	foldersChild := jsonListInner(foldersReq, "folders", []transform.Column{{Out: "node", Path: "name"}})
	projectsReq := httpx.Request{
		Method: "GET", URL: crm + "/projects",
		Query:        map[string]string{"parent": "{node}"},
		Continuation: httpx.Continuation{Kind: httpx.ContPaginate, NextTokenPath: "nextPageToken", TokenParam: "pageToken"},
	}
	return []plan.ExchangeSpec{
		plan.NewExchangeSpec("Folders", []string{"token", "org"}, []string{"node"},
			func(bound map[string]any) facade.Operator {
				seed := exchange.NewLiteralSource([]facade.Record{bind.NewDocRecord(map[string]any{
					"node": "organizations/" + fmt.Sprint(bound["org"]), "token": bound["token"],
				})}, 1)
				// folders → subfolders → … as a bounded fixpoint; token carries forward to each node.
				return descent.NewRecursive(0, seed, foldersChild,
					[]bind.Binding{bind.NewBinding("node", "parent"), bind.NewBinding("token", "token")},
					"node", termination.NewMaxIterations(64), 1)
			}, nil),
		// inner flatten: a node (org/folder) with no projects contributes nothing — it must not leak
		// an empty project into the bucket visitor (which would call ?project= and 400).
		plan.NewExchangeSpec("Projects", []string{"token", "node"}, []string{"project"},
			jsonListInner(projectsReq, "projects", []transform.Column{{Out: "project", Path: "projectId"}}),
			bind.NewInnerFlatten()),
	}
}

// blobEgress tags the provider, normalizes the raw per-provider signals to a consistent rendition,
// and projects to the uniform schema (null for absent fields, so every row is the same shape).
func blobEgress(provider string) []facade.Transform {
	return []facade.Transform{transform.NewConst(map[string]any{"provider": provider}), blobNormalize{}, bind.NewSelect(blobCols)}
}

// blobNormalize maps the raw per-provider signals onto a consistent cross-cloud rendition:
// key_management (who owns the key), and booleans public / versioning / https — so the same value
// means the same thing everywhere (crucially, AWS RestrictPublicBuckets and GCP
// publicAccessPrevention are inverted into "is it publicly accessible"). null = not surfaced.
type blobNormalize struct{}

func (blobNormalize) Apply(in facade.Page) (facade.Record, error) {
	row := map[string]any{}
	if doc, ok := in.Doc(facade.AnonymousPayload); ok {
		if m, ok := doc.(map[string]any); ok {
			for k, v := range m {
				row[k] = v
			}
		}
	}
	p := str(row["provider"])
	if p == "azure" {
		azureContainerGrain(row)
	}
	if row["name"] == nil {
		// Left-outer row: this scope (account / subscription / project) has NO bucket. Do not run
		// the classifiers over absent values — that fabricates attributes (e.g. public=true) for a
		// resource that doesn't exist. Leave every attribute null: an honest "scope, no buckets" row.
		row["encryption_class"] = nil
		row["public"] = nil
		row["versioning"] = nil
		row["https"] = nil
		row["access_logging"] = nil
		row["access_log_target"] = nil
		return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(row)}), nil
	}
	row["encryption_class"] = classifyKey(p, str(row["encryption_status"]))
	row["public"] = classifyPublic(p, row["public"])
	row["versioning"] = classifyVersioning(p, row["versioning"])
	row["https"] = classifyHTTPS(p, row["https"])
	row["access_logging"] = classifyLogging(p, row)
	return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(row)}), nil
}

// classifyLogging renders "is access logging on" from each provider's own signal. All three express it
// as a DESTINATION, so presence of a target is the answer: S3 names a target bucket, GCS a log bucket,
// Azure a diagnostic-setting sink. null only where the provider did not surface it at all.
// azureContainerGrain rewrites an Azure row from ACCOUNT grain to CONTAINER grain, so an Azure row
// names the same kind of thing as an AWS/GCS one. The account's own attributes (encryption, https,
// diagnostic logging) are properties of the account and are inherited by every container in it;
// public access is overridden by the container when it sets its own.
func azureContainerGrain(row map[string]any) {
	container := str(row["container"])
	if container == "" {
		// Account with no containers: keep the account row, but do not claim it is a bucket.
		row["name"] = nil
		return
	}
	row["name"] = str(row["name"]) + "/" + container
	if cp := str(row["container_public"]); cp != "" {
		// "None" means no anonymous access; "Blob"/"Container" both expose data publicly.
		row["public"] = !strings.EqualFold(cp, "None")
	}
}

func classifyLogging(provider string, row map[string]any) any {
	switch provider {
	case "aws", "gcp", "azure":
		if t := str(row["access_log_target"]); t != "" {
			return true
		}
		return false
	}
	return nil
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	}
	return false
}

// classifyPublic → true iff the object is publicly accessible (inverting AWS/GCP's "blocked" signals).
func classifyPublic(provider string, v any) any {
	switch provider {
	case "aws": // raw = RestrictPublicBuckets ("true" = blocked) or "open" (no block)
		return str(v) != "true"
	case "azure": // raw = allowBlobPublicAccess
		if v == nil {
			return nil
		}
		return truthy(v)
	case "gcp": // raw = publicAccessPrevention ("enforced" = blocked)
		return str(v) != "enforced"
	}
	return nil
}

// classifyVersioning → bool (azure not surfaced shallowly → null).
func classifyVersioning(provider string, v any) any {
	switch provider {
	case "aws":
		return str(v) == "Enabled"
	case "gcp":
		return truthy(v)
	}
	return nil
}

// classifyHTTPS → bool (GCP always TLS; AWS needs policy parsing, not surfaced shallowly → null).
func classifyHTTPS(provider string, v any) any {
	switch provider {
	case "azure":
		if v == nil {
			return nil
		}
		return truthy(v)
	case "gcp":
		return true
	}
	return nil
}

func classifyKey(provider, enc string) string {
	switch provider {
	case "aws":
		if enc == "" || enc == "none" || enc == "AES256" {
			return "provider-managed"
		}
		if strings.HasPrefix(enc, "aws:kms") {
			return "customer-managed"
		}
	case "azure":
		switch enc {
		case "Microsoft.Storage":
			return "provider-managed"
		case "Microsoft.Keyvault":
			return "customer-managed"
		}
	case "gcp":
		if enc == "" {
			return "provider-managed"
		}
		return "customer-managed"
	}
	return "unknown"
}

// NewAWSBlobEncryption: S3 buckets with encryption, versioning and public-access-block, each a
// per-bucket call fanned out over the bucket list.
func NewAWSBlobEncryption(id int64, region string, creds Credentials, endpoint string, w io.Writer) facade.Operator {
	return plan.Compose(id, AWSBlobPlan(region, creds, endpoint), w)
}

// AWSBlobPlan is the S3 bucket audit as a plan.Plan.
func AWSBlobPlan(region string, creds Credentials, endpoint string) plan.Plan {
	xml := transform.NewXMLToAgnostic()
	specs := []plan.ExchangeSpec{
		plan.NewExchangeSpec("ListBuckets", nil, []string{"name", "created", "arn", "region"},
			func(map[string]any) facade.Operator { return NewS3BucketRows(0, region, creds, endpoint) }, nil),
		plan.NewExchangeSpec("GetBucketEncryption", []string{"name", "region"}, []string{"status", "raw"},
			s3EncryptionInner(creds, region, endpoint), s3EncryptionFlatten()),
		plan.NewExchangeSpec("GetBucketVersioning", []string{"name", "region"}, []string{"status", "raw"},
			s3SubInner("versioning", creds, region, endpoint),
			httpx.NewStatusBranch(map[string]facade.Transform{
				"200": httpx.NewExtract(xml, map[string]string{"versioning": "VersioningConfiguration.Status"}, nil),
			}, nil)),
		plan.NewExchangeSpec("GetBucketLogging", []string{"name", "region"}, []string{"status", "raw"},
			s3SubInner("logging", creds, region, endpoint),
			httpx.NewStatusBranch(map[string]facade.Transform{
				// No <LoggingEnabled> element means logging is off — S3 returns 200 with an empty
				// BucketLoggingStatus rather than a 404, so absence is the signal.
				"200": httpx.NewExtract(xml, map[string]string{"access_log_target": "BucketLoggingStatus.LoggingEnabled.TargetBucket"}, nil),
			}, nil)),
		plan.NewExchangeSpec("GetBucketPublicAccess", []string{"name", "region"}, []string{"status", "raw"},
			s3SubInner("publicAccessBlock", creds, region, endpoint),
			httpx.NewStatusBranch(map[string]facade.Transform{
				"200": httpx.NewExtract(xml, map[string]string{"public": "PublicAccessBlockConfiguration.RestrictPublicBuckets"}, nil),
				"404": httpx.NewExtract(nil, nil, map[string]any{"public": "open"}), // no block configured
			}, nil)),
	}
	betas := []plan.BetaEdge{
		plan.NewBetaEdge("ListBuckets", "GetBucketEncryption", "name", "name"),
		plan.NewBetaEdge("ListBuckets", "GetBucketEncryption", "region", "region"),
		plan.NewBetaEdge("ListBuckets", "GetBucketVersioning", "name", "name"),
		plan.NewBetaEdge("ListBuckets", "GetBucketVersioning", "region", "region"),
		plan.NewBetaEdge("ListBuckets", "GetBucketLogging", "name", "name"),
		plan.NewBetaEdge("ListBuckets", "GetBucketLogging", "region", "region"),
		plan.NewBetaEdge("ListBuckets", "GetBucketPublicAccess", "name", "name"),
		plan.NewBetaEdge("ListBuckets", "GetBucketPublicAccess", "region", "region"),
	}
	return plan.NewPlan(specs, betas, nil, nil, blobEgress("aws"), encoder.NewJSONLEncoder())
}

// NewAzureBlobEncryption: storage accounts across every subscription. encryption/public/https all
// come from the account list (shallow); versioning is a blob-service property (not surfaced here).
func NewAzureBlobEncryption(id int64, endpoint, tenant, clientID, clientSecret string, w io.Writer) facade.Operator {
	specs := append([]plan.ExchangeSpec{
		plan.NewExchangeSpec("Token", []string{"tenant", "client_id", "client_secret"}, []string{"token"},
			httpx.MakeAgnostic(azureTokenReq(azureLoginBase(endpoint))),
			httpx.NewJSONExtract(map[string]string{"token": "access_token"})),
	}, azureStorageAuditSpecs(endpoint)...)
	betas := append(azureStorageAuditBetas(), azureStorageAuditTokenBetas("Token")...)
	inputs := map[string]any{"tenant": tenant, "client_id": clientID, "client_secret": clientSecret}
	return plan.Compose(id, plan.NewPlan(specs, betas, nil, inputs, blobEgress("azure"), encoder.NewJSONLEncoder()), w)
}

// azureStorageAuditSpecs is the Azure storage-account SCOPE+VISITOR: subscriptions the SP can read →
// storage accounts per subscription. Both bind {token}; how {token} is obtained (a native token
// exchange, a config-driven client-credentials exchange, or a static bearer κ) is the caller's choice.
// azureStorageAuditBetas are the edges INTERNAL to the storage audit. The caller adds the {token}
// fan-out, since it owns where the token comes from (a static bearer vs a token exchange).
func azureStorageAuditBetas() []plan.BetaEdge {
	return []plan.BetaEdge{
		plan.NewBetaEdge("Subscriptions", "StorageAccounts", "subscription_id", "subscription_id"),
		plan.NewBetaEdge("StorageAccounts", "DiagnosticSettings", "account_id", "account_id"),
		plan.NewBetaEdge("StorageAccounts", "Containers", "account_id", "account_id"),
		plan.NewBetaEdge("StorageAccounts", "Containers", "name", "name"),
	}
}

// azureStorageAuditTokenBetas fans {token} from src to every exchange in the audit that calls ARM.
func azureStorageAuditTokenBetas(src string) []plan.BetaEdge {
	return []plan.BetaEdge{
		plan.NewBetaEdge(src, "Subscriptions", "token", "token"),
		plan.NewBetaEdge(src, "StorageAccounts", "token", "token"),
		plan.NewBetaEdge(src, "DiagnosticSettings", "token", "token"),
		plan.NewBetaEdge(src, "Containers", "token", "token"),
	}
}

func azureStorageAuditSpecs(endpoint string) []plan.ExchangeSpec {
	mgmt := azureMgmtBase(endpoint)
	return []plan.ExchangeSpec{
		plan.NewExchangeSpec("Subscriptions", []string{"token"}, []string{"subscription_id"},
			azureList(mgmt+"/subscriptions?api-version="+azureSubsAPI,
				[]transform.Column{{Out: "subscription_id", Path: "subscriptionId"}}), nil),
		plan.NewExchangeSpec("StorageAccounts", []string{"token", "subscription_id"}, []string{"name", "account_id"},
			azureList(mgmt+"/subscriptions/{subscription_id}/providers/Microsoft.Storage/storageAccounts?api-version="+azureStorageAPI,
				[]transform.Column{
					{Out: "name", Path: "name"},
					{Out: "account_id", Path: "id"}, // the ARM resource path; diagnostic settings hang off it
					{Out: "encryption_status", Path: "properties.encryption.keySource"},
					{Out: "public", Path: "properties.allowBlobPublicAccess"},
					{Out: "https", Path: "properties.supportsHttpsTrafficOnly"},
				}), bind.NewInnerFlatten()), // a subscription with no storage accounts contributes no row
		// Azure has no "access logging" field on the account. The equivalent is whether the blob
		// service exports diagnostic logs, which lives in Azure Monitor — a per-account sub-resource
		// on the SAME management host and the SAME bearer, so it costs one extra call, not new auth.
		plan.NewExchangeSpec("DiagnosticSettings", []string{"token", "account_id"}, []string{"access_log_target"},
			azureList(mgmt+"{account_id}/blobServices/default/providers/Microsoft.Insights/diagnosticSettings?api-version="+azureDiagnosticAPI,
				[]transform.Column{
					{Out: "access_log_target", Path: "properties.storageAccountId"},
				}),
			// LEFT-OUTER deliberately: an account with no diagnostic settings is the common case and
			// the interesting one ("logging is off"). An inner join would delete exactly those rows.
			bind.NewTupleFlatten()),
		// The BUCKET analogue. An S3/GCS bucket is a blob CONTAINER, not a storage account — the
		// account is the level above (subscription → account → blob service → container). Auditing at
		// account grain would report a different kind of thing from the other clouds in the same
		// column. Container names are unique only WITHIN an account, so name is qualified
		// "<account>/<container>" to stay meaningful next to globally unique S3/GCS names.
		plan.NewExchangeSpec("Containers", []string{"token", "account_id", "name"}, []string{"container", "public"},
			azureList(mgmt+"{account_id}/blobServices/default/containers?api-version="+azureStorageAPI,
				[]transform.Column{
					{Out: "container", Path: "name"},
					// Container-level public access overrides the account's allowBlobPublicAccess.
					{Out: "container_public", Path: "properties.publicAccess"},
				}),
			// LEFT-OUTER: an account with no containers is still a real audit finding.
			bind.NewTupleFlatten()),
	}
}

// NewAzureBlobEncryptionAuth is the CONFIG-DRIVEN Azure storage-account audit: the auth method is
// chosen by AuthStruct, proving auth is switchable by JSON. client_credentials (Azure/Entra, generic
// OIDC) runs a token exchange (Auth → {token}, β to the audit); bearer (a pre-obtained OIDC token)
// injects a static {token} as a κ input. Downstream (Subscriptions → StorageAccounts, carrying
// Authorization: Bearer {token}) is identical — the only difference is where {token} comes from.
func NewAzureBlobEncryptionAuth(id int64, endpoint string, cfg auth.AuthStruct, w io.Writer) (facade.Operator, error) {
	p, err := AzureBlobAuthPlan(endpoint, cfg)
	if err != nil {
		return nil, err
	}
	return plan.Compose(id, p, w), nil
}

// AzureBlobAuthPlan builds the config-driven Azure storage-account audit as a plan.Plan (not yet
// composed), so callers can render it either as bytes (plan.Compose) or as a row cursor
// (plan.ComposeRows) — the pkg/omnisdk facade uses the latter. See NewAzureBlobEncryptionAuth.
func AzureBlobAuthPlan(endpoint string, cfg auth.AuthStruct) (plan.Plan, error) {
	m, err := auth.New(cfg)
	if err != nil {
		return nil, err
	}
	specs := azureStorageAuditSpecs(endpoint)
	betas := azureStorageAuditBetas()
	inputs := map[string]any{}

	if m.NeedsTokenExchange() {
		if m.Kind() != auth.KindClientCredentials {
			return nil, fmt.Errorf("azure auth: %s not supported here (use client_credentials or bearer)", m.Kind())
		}
		tr, _ := auth.AsOAuth(m)
		authSpec := plan.NewExchangeSpec("Auth", nil, []string{"token"},
			httpx.MakeAgnostic(auth.ClientCredentialsRequest(tr)),
			httpx.NewJSONExtract(map[string]string{"token": "access_token"}))
		specs = append([]plan.ExchangeSpec{authSpec}, specs...)
		betas = append(betas, azureStorageAuditTokenBetas("Auth")...)
	} else {
		tok, ok := auth.BearerToken(m)
		if !ok {
			return nil, fmt.Errorf("azure auth: %s not supported here (use client_credentials or bearer)", m.Kind())
		}
		inputs["token"] = tok // static bearer → κ, bound to the audit by name
	}
	return plan.NewPlan(specs, betas, nil, inputs, blobEgress("azure"), encoder.NewJSONLEncoder()), nil
}

// gcpOAuth is the service-account JWT → OAuth token exchange (scope varies by caller).
func gcpOAuth(endpoint string, creds GCPCredentials, scope string) (plan.ExchangeSpec, string) {
	tokenURL := gcpTokenURL(endpoint, creds)
	jwt, _ := creds.signedJWT(tokenURL, scope, time.Now())
	oauth := httpx.Request{
		Method: "POST", URL: tokenURL,
		Body: httpx.Body{Encoding: httpx.EncodingForm, Params: map[string]any{
			"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer", "assertion": "{assertion}",
		}},
	}
	return plan.NewExchangeSpec("OAuth", []string{"assertion"}, []string{"token"},
		httpx.MakeAgnostic(oauth), httpx.NewJSONExtract(map[string]string{"token": "access_token"})), jwt
}

// gcpListBucketsSpec is the per-project bucket query (the "visitor"): identical whether the project
// is a κ input (single) or bound from the project descent (org). project + token are In either way.
func gcpListBucketsSpec(endpoint string) plan.ExchangeSpec {
	list := httpx.Request{
		Method: "GET", URL: gcpStorageBase(endpoint) + "/b",
		Query:        map[string]string{"project": "{project}"},
		Continuation: httpx.Continuation{Kind: httpx.ContPaginate, NextTokenPath: "nextPageToken", TokenParam: "pageToken"},
	}
	return plan.NewExchangeSpec("ListBuckets", []string{"token", "project"}, []string{"name"},
		jsonListInner(list, "items", []transform.Column{
			{Out: "name", Path: "name"},
			{Out: "encryption_status", Path: "encryption.defaultKmsKeyName"},
			{Out: "public", Path: "iamConfiguration.publicAccessPrevention"},
			{Out: "versioning", Path: "versioning.enabled"},
			// GCS carries usage/storage log config on the bucket resource itself — no extra call.
			{Out: "access_log_target", Path: "logging.logBucket"},
		}), bind.NewInnerFlatten()) // a project with no buckets contributes no row
}

// NewGCPBlobEncryption: cloud-storage buckets in a SINGLE project (the project is a required κ input,
// never inferred). encryption/public/versioning come from the bucket list (shallow); GCP always
// enforces TLS, so https is constant.
func NewGCPBlobEncryption(id int64, endpoint string, creds GCPCredentials, project string, w io.Writer) facade.Operator {
	return plan.Compose(id, GCPBlobPlan(endpoint, creds, project), w)
}

// GCPBlobPlan is the single-project GCP bucket audit as a plan.Plan (REST).
func GCPBlobPlan(endpoint string, creds GCPCredentials, project string) plan.Plan {
	oauth, jwt := gcpOAuth(endpoint, creds, gcpStorageScope)
	specs := []plan.ExchangeSpec{oauth, gcpListBucketsSpec(endpoint)}
	betas := []plan.BetaEdge{plan.NewBetaEdge("OAuth", "ListBuckets", "token", "token")}
	inputs := map[string]any{"assertion": jwt, "project": project}
	return plan.NewPlan(specs, betas, nil, inputs, blobEgress("gcp"), encoder.NewJSONLEncoder())
}

// NewGCPBlobEncryptionOrg audits buckets across an ENTIRE org (org is a required κ input — scope is
// explicit, never "whatever this SA can see"). It is the reusable org scope-source
// (gcpOrgProjectSpecs: recursive folder descent → every project, folder-nested included) with the
// bucket query appended as the VISITOR. Swap gcpListBucketsSpec for a VM / network / DB spec (bound
// on project+token) to audit those instead — the descent is identical.
func NewGCPBlobEncryptionOrg(id int64, endpoint string, creds GCPCredentials, org string, w io.Writer) facade.Operator {
	return plan.Compose(id, GCPBlobOrgPlan(endpoint, creds, org), w)
}

// GCPBlobOrgPlan is the org-wide GCP bucket audit as a plan.Plan (REST folder→project descent).
func GCPBlobOrgPlan(endpoint string, creds GCPCredentials, org string) plan.Plan {
	oauth, jwt := gcpOAuth(endpoint, creds, gcpCloudPlatformScope)
	specs := append([]plan.ExchangeSpec{oauth}, gcpOrgProjectSpecs(endpoint)...)
	specs = append(specs, gcpListBucketsSpec(endpoint))
	betas := []plan.BetaEdge{
		plan.NewBetaEdge("OAuth", "Folders", "token", "token"),
		plan.NewBetaEdge("OAuth", "Projects", "token", "token"),
		plan.NewBetaEdge("OAuth", "ListBuckets", "token", "token"),
		plan.NewBetaEdge("Folders", "Projects", "node", "node"),
		plan.NewBetaEdge("Projects", "ListBuckets", "project", "project"),
	}
	inputs := map[string]any{"assertion": jwt, "org": org}
	return plan.NewPlan(specs, betas, nil, inputs, blobEgress("gcp"), encoder.NewJSONLEncoder())
}

// AzureCreds bundles the Azure SP credentials for the all-in-one factory.
type AzureCreds struct{ Tenant, ClientID, ClientSecret string }

// NewBlobAuditShallow runs all three provider audits as disjoint DAGs into w, each under its own
// admission controller (perProvider concurrent requests per backend).
func NewBlobAuditShallow(id int64, endpoint string, aws Credentials, awsRegion string, az AzureCreds, gcp GCPCredentials, gcpProject string, perProvider int, w io.Writer) facade.Operator {
	return schedule.NewMulti([]schedule.Sub{
		{Op: NewAWSBlobEncryption(id, awsRegion, aws, endpoint, w), Admissions: admit.PerScope(perProvider)},
		{Op: NewAzureBlobEncryption(id, endpoint, az.Tenant, az.ClientID, az.ClientSecret, w), Admissions: admit.PerScope(perProvider)},
		{Op: NewGCPBlobEncryption(id, endpoint, gcp, gcpProject, w), Admissions: admit.PerScope(perProvider)},
	})
}

// NewBlobAuditOrg is the org-wide all-providers audit: three disjoint DAGs under separate
// controllers, going as wide as each provider allows. Azure already spans every subscription the SP
// can read; GCP recurses the whole org (gcpOrg); AWS is the creds' single account — org-wide AWS
// (Organizations + STS assume-role per account) is not built yet, so it stays account-scoped here.
func NewBlobAuditOrg(id int64, endpoint string, aws Credentials, awsRegion string, az AzureCreds, gcp GCPCredentials, gcpOrg string, perProvider int, w io.Writer) facade.Operator {
	return schedule.NewMulti([]schedule.Sub{
		{Op: NewAWSBlobEncryption(id, awsRegion, aws, endpoint, w), Admissions: admit.PerScope(perProvider)},
		{Op: NewAzureBlobEncryption(id, endpoint, az.Tenant, az.ClientID, az.ClientSecret, w), Admissions: admit.PerScope(perProvider)},
		{Op: NewGCPBlobEncryptionOrg(id, endpoint, gcp, gcpOrg, w), Admissions: admit.PerScope(perProvider)},
	})
}
