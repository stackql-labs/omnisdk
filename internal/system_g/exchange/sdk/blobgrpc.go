package sdk

import (
	"io"

	"google.golang.org/grpc"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/grpcx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"
)

// grpcBucketInner lists buckets over gRPC (dynamic, descriptor-driven via grpcx) and projects the
// response to the uniform audit columns — the exact analogue of the REST jsonListInner, differing
// only in transport. grpcx emits the response as an already-decoded doc, so no JSON decode step is
// needed before the projection.
func grpcBucketInner(d *grpcx.Descriptors, target string, dialOpts []grpc.DialOption) func(map[string]any) facade.Operator {
	send := grpcx.Make(d, grpcx.Request{
		Target: target,
		Method: "google.storage.v2.Storage.ListBuckets",
		Fields: map[string]any{"parent": "projects/{project}"},
		Metadata: map[string]string{
			"authorization": "Bearer {token}",
			// google.api.routing: routing_parameters{ field:"parent" path_template:"{project=**}" }
			"x-goog-request-params": "project=projects%2F{project}",
		},
		Continuation: grpcx.Continuation{PageTokenField: "page_token", NextTokenPath: "nextPageToken"},
	}, dialOpts...)
	cols := []transform.Column{
		{Out: "name", Path: "bucketId"},
		{Out: "encryption_status", Path: "encryption.defaultKmsKey"},
		{Out: "public", Path: "iamConfig.publicAccessPrevention"},
		{Out: "versioning", Path: "versioning.enabled"},
	}
	return func(bound map[string]any) facade.Operator {
		projected := exchange.NewTransformExchange(0, send(bound), transform.NewProjection("buckets", cols), 1)
		return exchange.NewExplodeRows(projected, 1)
	}
}

// grpcBucketSpec is the gRPC bucket-listing visitor (bound on project+token) — the transport-swapped
// analogue of gcpListBucketsSpec, usable at the leaf of either the single-project or org descent.
func grpcBucketSpec(d *grpcx.Descriptors, target string, dialOpts []grpc.DialOption) plan.ExchangeSpec {
	return plan.NewExchangeSpec("ListBuckets", []string{"token", "project"}, []string{"name"},
		grpcBucketInner(d, target, dialOpts), bind.NewInnerFlatten()) // a project with no buckets contributes no row
}

// NewGCPBlobEncryptionGRPC is the gRPC analogue of the GCP bucket audit: it hits a protobuf Storage
// endpoint instead of REST, yet runs the SAME egress (blobEgress "gcp") — so an equivalent query
// yields equivalent normalized rows. Auth is the SAME as the REST GCP commands: the service-account
// key signs a JWT, OAuth (over HTTP) exchanges it for a token, and β(token) carries it to the gRPC
// call as the bearer. endpoint overrides the OAuth token URL (real GCS: ""); dialOpts carry TLS/creds
// for the gRPC target (tests inject a bufconn dialer). No existing code is touched.
func NewGCPBlobEncryptionGRPC(id int64, d *grpcx.Descriptors, endpoint, target string, creds GCPCredentials, project string, w io.Writer, dialOpts ...grpc.DialOption) facade.Operator {
	return plan.Compose(id, GCPBlobGRPCPlan(d, endpoint, target, creds, project, dialOpts...), w)
}

// GCPBlobGRPCPlan is the single-project gRPC bucket audit as a plan.Plan (compose as bytes or as
// rows): OAuth (SA JWT → token) → gRPC ListBuckets(bearer). egress is the shared blobEgress("gcp").
func GCPBlobGRPCPlan(d *grpcx.Descriptors, endpoint, target string, creds GCPCredentials, project string, dialOpts ...grpc.DialOption) plan.Plan {
	oauth, jwt := gcpOAuth(endpoint, creds, gcpStorageScope)
	specs := []plan.ExchangeSpec{oauth, grpcBucketSpec(d, target, dialOpts)}
	betas := []plan.BetaEdge{plan.NewBetaEdge("OAuth", "ListBuckets", "token", "token")}
	inputs := map[string]any{"assertion": jwt, "project": project}
	return plan.NewPlan(specs, betas, nil, inputs, blobEgress("gcp"), encoder.NewJSONLEncoder())
}

// NewGCPBlobEncryptionOrgGRPC is the ORG-WIDE gRPC bucket audit: the SAME recursive folder→project
// descent as the REST org audit (gcpOrgProjectSpecs, over REST/CRM), with the gRPC bucket visitor
// swapped in for the REST one — proving the descent is transport-agnostic (only the leaf changes).
// org is a required κ input; egress is the shared blobEgress("gcp").
func NewGCPBlobEncryptionOrgGRPC(id int64, d *grpcx.Descriptors, endpoint, target string, creds GCPCredentials, org string, w io.Writer, dialOpts ...grpc.DialOption) facade.Operator {
	return plan.Compose(id, GCPBlobOrgGRPCPlan(d, endpoint, target, creds, org, dialOpts...), w)
}

// GCPBlobOrgGRPCPlan is the org-wide gRPC bucket audit as a plan.Plan (REST folder→project descent +
// gRPC bucket visitor). org is a required κ input; egress is the shared blobEgress("gcp").
func GCPBlobOrgGRPCPlan(d *grpcx.Descriptors, endpoint, target string, creds GCPCredentials, org string, dialOpts ...grpc.DialOption) plan.Plan {
	oauth, jwt := gcpOAuth(endpoint, creds, gcpCloudPlatformScope)
	specs := append([]plan.ExchangeSpec{oauth}, gcpOrgProjectSpecs(endpoint)...)
	specs = append(specs, grpcBucketSpec(d, target, dialOpts))
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
