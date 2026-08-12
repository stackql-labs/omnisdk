package sdk

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
	ep "github.com/stackql-labs/omnisdk/internal/system_g/endpoint"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/transform"
)

// AWS exchange factories. Everything here is provider config — request configs, URLs, and the
// signer — fed to the canonical httpx/plan/transform constructors. The only AWS building logic
// (SigV4 signing) lives in sigv4.go.

// ---- S3 URL construction ----------------------------------------------------
// With no endpoint override, real AWS endpoints are used; with an override (a mock) path-style
// addressing is used so a single host serves every bucket.

func s3ListBase(endpoint, region string) string {
	return resolve(endpoint, ep.AWSS3, map[string]string{"region": region}) + "/"
}

func s3EncryptionURL(endpoint, region, bucket string) string {
	return s3SubURL(endpoint, region, bucket, "encryption")
}

// s3SubURL is a bucket sub-resource URL (?encryption, ?versioning, ?publicAccessBlock, …).
func s3SubURL(endpoint, region, bucket, sub string) string {
	base := resolve(endpoint, ep.AWSS3, map[string]string{"region": region})
	if overridden(endpoint, ep.AWSS3) {
		// A redirected S3 is one host serving every bucket, so buckets address PATH-style. Virtual-host
		// addressing (bucket as a subdomain) only works against the real service.
		return base + "/" + bucket + "?" + sub
	}
	return insertBucketHost(base, bucket) + "/?" + sub
}

// insertBucketHost turns https://s3.<region>.amazonaws.com into the bucket's virtual host,
// https://<bucket>.s3.<region>.amazonaws.com — S3's real addressing.
func insertBucketHost(base, bucket string) string {
	const scheme = "https://"
	if strings.HasPrefix(base, scheme) {
		return scheme + bucket + "." + strings.TrimPrefix(base, scheme)
	}
	return base + "/" + bucket
}

// s3SubInner fetches a bucket sub-resource for one bound bucket, emitting raw {status, raw}.
func s3SubInner(sub string, creds Credentials, defaultRegion, endpoint string) func(bound map[string]any) facade.Operator {
	return func(bound map[string]any) facade.Operator {
		name := str(bound["name"])
		region := str(bound["region"])
		if region == "" {
			region = defaultRegion
		}
		req := httpx.Request{Method: "GET", URL: s3SubURL(endpoint, region, name, sub)}
		return httpx.MakeAgnostic(req, NewSigV4Transform(s3Signer(region, creds)))(bound)
	}
}

func s3Signer(region string, creds Credentials) Signer {
	// S3 requires a signed X-Amz-Content-Sha256.
	return NewSigV4Signer(region, "s3", creds, true)
}

// ---- S3 ListBuckets ---------------------------------------------------------

// NewS3ListBuckets lists buckets as a generic httpx exchange: a paginated GET whose next page is
// driven by the XML <ContinuationToken> (its absence marks the last page). Each page's raw XML is
// emitted under facade.AnonymousPayload. The XML decoder (mxj) is injected as a drop-in transform
// so the continuation token is read off the agnostic doc, not by welding XML into httpx.
func NewS3ListBuckets(id int64, region string, creds Credentials, endpoint string, pageSize int) facade.Operator {
	req := httpx.Request{
		Method: "GET",
		URL:    s3ListBase(endpoint, region),
		Query:  map[string]string{"max-buckets": strconv.Itoa(clampMaxBuckets(pageSize))},
		Continuation: httpx.Continuation{
			Kind:          httpx.ContPaginate,
			NextTokenPath: "ListAllMyBucketsResult.ContinuationToken",
			TokenParam:    "continuation-token",
		},
	}
	return httpx.Make(req, transform.NewXMLToAgnostic(), NewSigV4Transform(s3Signer(region, creds)))(nil)
}

// clampMaxBuckets keeps pageSize within S3's valid 1..10000 range (default 1000).
func clampMaxBuckets(n int) int {
	switch {
	case n < 1:
		return 1000
	case n > 10000:
		return 10000
	default:
		return n
	}
}

// NewS3ListBucketsGraph lists buckets as JSONL via the build→select→compose layer: one declared
// exchange with the XML→agnostic→projection presentation on the egress path.
func NewS3ListBucketsGraph(id int64, region string, creds Credentials, endpoint string, w io.Writer) facade.Operator {
	return plan.Compose(id, S3ListPlan(id, region, creds, endpoint), w)
}

// S3ListPlan is NewS3ListBucketsGraph as an uncomposed plan.Plan, so callers can render it as bytes
// (plan.Compose) or as a row cursor (plan.ComposeRows) — the pkg/omnisdk facade uses the latter.
func S3ListPlan(id int64, region string, creds Credentials, endpoint string) plan.Plan {
	specs := []plan.ExchangeSpec{
		plan.NewExchangeSpec("ListBuckets", nil, []string{"name", "created", "arn"},
			func(map[string]any) facade.Operator {
				return NewS3ListBuckets(id, region, creds, endpoint, 1000) // paginate, all pages
			}, nil),
	}
	egress := []facade.Transform{
		transform.NewXMLToAgnostic(),
		transform.NewProjection("ListAllMyBucketsResult.Buckets.Bucket", []transform.Column{
			{Out: "name", Path: "Name"},
			{Out: "created", Path: "CreationDate"},
			{Out: "arn", Path: "BucketArn"},
		}),
	}
	return plan.NewPlan(specs, nil, nil, nil, egress, encoder.NewJSONLEncoder())
}

// ---- S3 buckets ⋈ encryption (the bowtie) -----------------------------------

// NewS3BucketRows is the outer of the bind join: paginated ListBuckets → agnostic → project
// (name, created, arn, region) → explode into one record per bucket.
func NewS3BucketRows(id int64, region string, creds Credentials, endpoint string) facade.Operator {
	list := NewS3ListBuckets(id, region, creds, endpoint, 1000)
	decoded := exchange.NewTransformExchange(id+1, list, transform.NewXMLToAgnostic(), 1)
	projected := exchange.NewTransformExchange(id+2, decoded,
		transform.NewProjection("ListAllMyBucketsResult.Buckets.Bucket", []transform.Column{
			{Out: "name", Path: "Name"},
			{Out: "created", Path: "CreationDate"},
			{Out: "arn", Path: "BucketArn"},
			{Out: "region", Path: "BucketRegion"},
		}), 1)
	return exchange.NewExplodeRows(projected, 1)
}

// NewS3BucketsWithEncryption is the bowtie: two declared exchanges joined by β(name, region).
// Each listed bucket drives a per-bucket GetBucketEncryption; the sink flattens the (input, raw)
// tuple into one JSONL row. Left-outer: buckets with no encryption still appear.
//
//	ListBuckets ──β(name,region)──▶ GetBucketEncryption ──▶ flatten ──▶ JSONL(w)
func NewS3BucketsWithEncryption(id int64, region string, creds Credentials, endpoint string, w io.Writer) facade.Operator {
	return plan.Compose(id, S3EncryptionPlan(id, region, creds, endpoint), w)
}

// S3EncryptionPlan is the bowtie as an uncomposed plan.Plan (see S3ListPlan for why).
func S3EncryptionPlan(id int64, region string, creds Credentials, endpoint string) plan.Plan {
	specs := []plan.ExchangeSpec{
		plan.NewExchangeSpec("ListBuckets", nil, []string{"name", "created", "arn", "region"},
			func(map[string]any) facade.Operator { return NewS3BucketRows(id, region, creds, endpoint) }, nil),
		plan.NewExchangeSpec("GetBucketEncryption", []string{"name", "region"}, []string{"status", "raw"},
			s3EncryptionInner(creds, region, endpoint),
			s3EncryptionFlatten()), // canonical decode + status branch (config only)
	}
	betas := []plan.BetaEdge{
		plan.NewBetaEdge("ListBuckets", "GetBucketEncryption", "name", "name"),
		plan.NewBetaEdge("ListBuckets", "GetBucketEncryption", "region", "region"),
	}
	return plan.NewPlan(specs, betas, nil, nil, nil, encoder.NewJSONLEncoder())
}

// s3EncryptionInner fetches GetBucketEncryption for one bound bucket and emits the raw
// {status, raw} response; decode + branch happen at the sink (s3EncryptionFlatten).
func s3EncryptionInner(creds Credentials, defaultRegion, endpoint string) func(bound map[string]any) facade.Operator {
	return func(bound map[string]any) facade.Operator {
		name := str(bound["name"])
		region := str(bound["region"])
		if region == "" {
			region = defaultRegion
		}
		req := httpx.Request{Method: "GET", URL: s3EncryptionURL(endpoint, region, name)}
		return httpx.MakeAgnostic(req, NewSigV4Transform(s3Signer(region, creds)))(bound)
	}
}

// s3EncryptionRule is the XML path to the (single) SSE rule in a GetBucketEncryption response.
const s3EncryptionRule = "ServerSideEncryptionConfiguration.Rule"

// s3EncryptionFlatten is the canonical decode+branch for GetBucketEncryption: 200 → pull the SSE
// algorithm + optional kms key / bucket-key flag off the XML (nested under "encryption"); 404 →
// "none"; anything else → "error". No bespoke code — httpx.NewStatusBranch (the response-code
// behavioural edge) over httpx.NewExtract (XML decode + path extract), configured with S3 paths.
func s3EncryptionFlatten() facade.Transform {
	return httpx.NewStatusBranch(map[string]facade.Transform{
		"200": httpx.NewExtract(transform.NewXMLToAgnostic(), map[string]string{
			"encryption_status":             s3EncryptionRule + ".ApplyServerSideEncryptionByDefault.SSEAlgorithm",
			"encryption.algorithm":          s3EncryptionRule + ".ApplyServerSideEncryptionByDefault.SSEAlgorithm",
			"encryption.kms_key_id":         s3EncryptionRule + ".ApplyServerSideEncryptionByDefault.KMSMasterKeyID",
			"encryption.bucket_key_enabled": s3EncryptionRule + ".BucketKeyEnabled",
		}, nil),
		"404": httpx.NewExtract(nil, nil, map[string]any{"encryption_status": "none"}),
	}, httpx.NewExtract(nil, nil, map[string]any{"encryption_status": "error"}))
}

// ---- EC2 (Query API over POST) ----------------------------------------------
// EC2's Query API rides params on the request; we POST them as a form body (not GET query) so
// large parameter sets can't hit a URL-length limit. httpx form-encodes and SigV4 signs the body.

const ec2Version = "2016-11-15"

func ec2Signer(region string, creds Credentials) Signer {
	return NewSigV4Signer(region, "ec2", creds, false)
}

func ec2Base(endpoint, region string) string {
	return resolve(endpoint, ep.AWSEC2, map[string]string{"region": region}) + "/"
}

// ec2Params builds the shared Query-API params for a create action tagged with description.
func ec2Params(action, cidr, resourceType, description string) map[string]any {
	return map[string]any{
		"Action":                          action,
		"Version":                         ec2Version,
		"CidrBlock":                       cidr,
		"TagSpecification.1.ResourceType": resourceType,
		"TagSpecification.1.Tag.1.Key":    "Description",
		"TagSpecification.1.Tag.1.Value":  description,
	}
}

// ec2VpcReq templates {vpc_cidr} into the form body — bound at runtime from the required κ input.
func ec2VpcReq(endpoint, region, description string) httpx.Request {
	return httpx.Request{
		Method: "POST",
		URL:    ec2Base(endpoint, region),
		Body:   httpx.Body{Encoding: httpx.EncodingForm, Params: ec2Params("CreateVpc", "{vpc_cidr}", "vpc", description)},
	}
}

// ec2SubnetReq templates {subnet_cidr} (required κ input) and {vpc_id} (bound from CreateVpc's
// output) into the form body.
func ec2SubnetReq(endpoint, region, description string) httpx.Request {
	p := ec2Params("CreateSubnet", "{subnet_cidr}", "subnet", description)
	p["VpcId"] = "{vpc_id}"
	return httpx.Request{
		Method: "POST",
		URL:    ec2Base(endpoint, region),
		Body:   httpx.Body{Encoding: httpx.EncodingForm, Params: p},
	}
}

// ---- AWS provision (VPC then subnet) ----------------------------------------

// NewProvision provisions a VPC then a subnet in it. Both creates are generic httpx exchanges
// (config, not code): a request config sent+signed by httpx, extracted by the canonical
// httpx.NewStatusBranch{200: NewExtract(XML)} — the same shape as the GCP provision, differing
// only in provider config. vpcCidr/subnetCidr are mandatory κ inputs (declared in each create's
// In, templated into its form body); plan.Validate rejects the plan instantly if either is empty.
// They seed the run so both creates are staged, and are projected away by the egress select.
// Writes: FormCreate. Creates REAL AWS resources.
//
//	CreateVpc ──β(vpc_id), E_α{100ms}──▶ CreateSubnet ──▶ project ──▶ JSONL(w)
func NewProvision(id int64, region string, creds Credentials, endpoint, vpcCidr, subnetCidr string, w io.Writer) facade.Operator {
	return plan.Compose(id, AWSProvisionPlan(id, region, creds, endpoint, vpcCidr, subnetCidr), w)
}

// AWSProvisionPlan is NewProvision as an uncomposed plan.Plan (see S3ListPlan for why).
func AWSProvisionPlan(id int64, region string, creds Credentials, endpoint, vpcCidr, subnetCidr string) plan.Plan {
	stamp := time.Now().UTC().Format(time.RFC3339)
	vpcDesc := "omnisdk vpc " + stamp
	subnetDesc := "omnisdk subnet " + stamp
	sign := NewSigV4Transform(ec2Signer(region, creds))

	specs := []plan.ExchangeSpec{
		plan.NewExchangeSpec("CreateVpc", []string{"vpc_cidr"}, []string{"vpc_id", "vpc_description"},
			httpx.MakeAgnostic(ec2VpcReq(endpoint, region, vpcDesc), sign),
			httpx.NewStatusBranch(map[string]facade.Transform{
				"200": httpx.NewExtract(transform.NewXMLToAgnostic(),
					map[string]string{"vpc_id": "CreateVpcResponse.vpc.vpcId"},
					map[string]any{"vpc_description": vpcDesc}),
			}, nil)),
		plan.NewExchangeSpec("CreateSubnet", []string{"vpc_id", "subnet_cidr"}, []string{"subnet_id", "subnet_description"},
			httpx.MakeAgnostic(ec2SubnetReq(endpoint, region, subnetDesc), sign),
			httpx.NewStatusBranch(map[string]facade.Transform{
				"200": httpx.NewExtract(transform.NewXMLToAgnostic(),
					map[string]string{"subnet_id": "CreateSubnetResponse.subnet.subnetId"},
					map[string]any{"subnet_description": subnetDesc}),
			}, nil)),
	}
	betas := []plan.BetaEdge{plan.NewBetaEdge("CreateVpc", "CreateSubnet", "vpc_id", "vpc_id")}
	// α late-bound: a 100ms delay on the CreateVpc→CreateSubnet edge.
	alphas := []plan.AlphaEdge{plan.NewAlphaEdge("CreateVpc", "CreateSubnet", bind.NewDelayEdge(100*time.Millisecond))}
	inputs := map[string]any{"vpc_cidr": vpcCidr, "subnet_cidr": subnetCidr}
	egress := []facade.Transform{bind.NewSelect([]string{"vpc_id", "vpc_description", "subnet_id", "subnet_description"})}

	return plan.NewPlan(specs, betas, alphas, inputs, egress, encoder.NewJSONLEncoder())
}

// str renders any agnostic value as a string ("" for nil).
func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
