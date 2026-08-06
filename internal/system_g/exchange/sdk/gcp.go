package sdk

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
)

// GCP. Auth is an exchange: a service-account JWT (RS256) is exchanged for an OAuth2 access
// token, which flows via β(token) to every authenticated exchange as a Bearer header. The only
// building logic here is the JWT signing + credential parsing; everything else is config fed to
// the canonical httpx/plan constructors.

const (
	gcpComputeScope = "https://www.googleapis.com/auth/compute"
	gcpSubnetCidr   = "10.0.0.0/24"
	gcpPollInterval = 1 * time.Second
	gcpPollAttempts = 60
)

// ---- Credentials + JWT (building logic) -------------------------------------

// GCPCredentials is a parsed service-account key. The RSA key is parsed once at load so token
// signing cannot fail on a malformed key mid-flow.
type GCPCredentials struct {
	ClientEmail string
	TokenURI    string
	ProjectID   string
	key         *rsa.PrivateKey
}

// ParseGCPCredentials parses a service-account JSON key (as written by gcloud).
func ParseGCPCredentials(saJSON []byte) (GCPCredentials, error) {
	var raw struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
		ProjectID   string `json:"project_id"`
	}
	if err := json.Unmarshal(saJSON, &raw); err != nil {
		return GCPCredentials{}, fmt.Errorf("gcp: bad service-account json: %w", err)
	}
	block, _ := pem.Decode([]byte(raw.PrivateKey))
	if block == nil {
		return GCPCredentials{}, fmt.Errorf("gcp: no PEM private_key in service-account json")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return GCPCredentials{}, fmt.Errorf("gcp: parse private key: %w", err)
	}
	rsaKey, ok := k.(*rsa.PrivateKey)
	if !ok {
		return GCPCredentials{}, fmt.Errorf("gcp: private key is not RSA")
	}
	if raw.TokenURI == "" {
		raw.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return GCPCredentials{ClientEmail: raw.ClientEmail, TokenURI: raw.TokenURI, ProjectID: raw.ProjectID, key: rsaKey}, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signedJWT builds and RS256-signs the assertion for the jwt-bearer grant.
func (c GCPCredentials) signedJWT(aud, scope string, now time.Time) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss":   c.ClientEmail,
		"scope": scope,
		"aud":   aud,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	input := b64url(header) + "." + b64url(claims)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("gcp: sign jwt: %w", err)
	}
	return input + "." + b64url(sig), nil
}

// ---- Endpoints (config) -----------------------------------------------------

// gcpTokenURL resolves the token endpoint (override → path-style mock).
func gcpTokenURL(endpoint string, creds GCPCredentials) string {
	if endpoint != "" {
		return strings.TrimRight(endpoint, "/") + "/token"
	}
	return creds.TokenURI
}

// gcpComputeBase is the compute endpoint up to /projects (project supplied via {project} in the
// URL template). Override → path-style for the mock.
func gcpComputeBase(endpoint string) string {
	if endpoint != "" {
		return strings.TrimRight(endpoint, "/") + "/compute/v1/projects"
	}
	return "https://compute.googleapis.com/compute/v1/projects"
}

// ---- Provision (config) -----------------------------------------------------

// NewGCPProvision provisions a GCP VPC network then a subnetwork in it. Every exchange is a data
// instance of the generic httpx exchange (config, not code): OAuth, the two async creates, and
// the two polls are all httpx.Request values; extraction is the canonical httpx.NewJSONExtract.
// The only GCP-specific code is the JWT signing (seeded as a κ input) and these configs.
//
//	OAuth ─β(token)→ CreateNetwork ─β(net_op)→ PollNetwork ─β(network_link)→ CreateSubnet ─β(subnet_op)→ PollSubnet → project → JSONL(w)
//
// project/region are mandatory κ inputs; the JWT assertion is a κ input computed once here.
func NewGCPProvision(id int64, region string, creds GCPCredentials, endpoint, project string, w io.Writer) facade.Operator {
	return plan.Compose(id, GCPProvisionPlan(id, region, creds, endpoint, project), w)
}

// GCPProvisionPlan is NewGCPProvision as an uncomposed plan.Plan (see S3ListPlan for why).
func GCPProvisionPlan(id int64, region string, creds GCPCredentials, endpoint, project string) plan.Plan {
	stamp := time.Now().UTC().Format(time.RFC3339)
	netName := "omnisdk-net-" + gcpStampSuffix(stamp)
	subnetName := "omnisdk-subnet-" + gcpStampSuffix(stamp)

	tokenURL := gcpTokenURL(endpoint, creds)
	base := gcpComputeBase(endpoint)
	jwt, _ := creds.signedJWT(tokenURL, gcpComputeScope, time.Now())

	oauth := httpx.Request{
		Method: "POST", URL: tokenURL,
		Body: httpx.Body{Encoding: httpx.EncodingForm, Params: map[string]any{
			"grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer",
			"assertion":  "{assertion}",
		}},
	}
	createNet := httpx.Request{
		Method: "POST", URL: base + "/{project}/global/networks",
		Body: httpx.Body{Encoding: httpx.EncodingJSON, Params: map[string]any{
			"name": netName, "autoCreateSubnetworks": false, "description": "omnisdk network " + stamp,
		}},
		Headers: map[string]string{"Authorization": "Bearer {token}"},
	}
	pollNet := httpx.Request{
		Method: "GET", URL: "{net_op}",
		Headers:      map[string]string{"Authorization": "Bearer {token}"},
		Continuation: gcpPoll(),
	}
	createSubnet := httpx.Request{
		Method: "POST", URL: base + "/{project}/regions/{region}/subnetworks",
		Body: httpx.Body{Encoding: httpx.EncodingJSON, Params: map[string]any{
			"name": subnetName, "network": "{network_link}", "ipCidrRange": gcpSubnetCidr,
			"description": "omnisdk subnet " + stamp,
		}},
		Headers: map[string]string{"Authorization": "Bearer {token}"},
	}
	pollSubnet := httpx.Request{
		Method: "GET", URL: "{subnet_op}",
		Headers:      map[string]string{"Authorization": "Bearer {token}"},
		Continuation: gcpPoll(),
	}

	specs := []plan.ExchangeSpec{
		plan.NewExchangeSpec("OAuth", []string{"assertion"}, []string{"token"},
			httpx.MakeAgnostic(oauth), httpx.NewJSONExtract(map[string]string{"token": "access_token"})),
		plan.NewExchangeSpec("CreateNetwork", []string{"token", "project"}, []string{"net_op"},
			httpx.MakeAgnostic(createNet), httpx.NewJSONExtract(map[string]string{"net_op": "selfLink"})),
		plan.NewExchangeSpec("PollNetwork", []string{"token", "net_op"}, []string{"vpc_id", "network_link"},
			httpx.MakeAgnostic(pollNet), httpx.NewJSONExtract(map[string]string{"vpc_id": "targetId", "network_link": "targetLink"})),
		plan.NewExchangeSpec("CreateSubnet", []string{"token", "network_link", "project", "region"}, []string{"subnet_op"},
			httpx.MakeAgnostic(createSubnet), httpx.NewJSONExtract(map[string]string{"subnet_op": "selfLink"})),
		plan.NewExchangeSpec("PollSubnet", []string{"token", "subnet_op"}, []string{"subnet_id", "subnet_link"},
			httpx.MakeAgnostic(pollSubnet), httpx.NewJSONExtract(map[string]string{"subnet_id": "targetId", "subnet_link": "targetLink"})),
	}
	betas := []plan.BetaEdge{
		plan.NewBetaEdge("OAuth", "CreateNetwork", "token", "token"),
		plan.NewBetaEdge("OAuth", "PollNetwork", "token", "token"),
		plan.NewBetaEdge("CreateNetwork", "PollNetwork", "net_op", "net_op"),
		plan.NewBetaEdge("OAuth", "CreateSubnet", "token", "token"),
		plan.NewBetaEdge("PollNetwork", "CreateSubnet", "network_link", "network_link"),
		plan.NewBetaEdge("OAuth", "PollSubnet", "token", "token"),
		plan.NewBetaEdge("CreateSubnet", "PollSubnet", "subnet_op", "subnet_op"),
	}
	inputs := map[string]any{"project": project, "region": region, "assertion": jwt}
	egress := []facade.Transform{bind.NewSelect([]string{"vpc_id", "network_link", "subnet_id", "subnet_link"})}
	return plan.NewPlan(specs, betas, nil, inputs, egress, encoder.NewJSONLEncoder())
}

func gcpPoll() httpx.Continuation {
	return httpx.Continuation{
		Kind: httpx.ContPoll, StatusPath: "status", DoneValue: "DONE",
		Interval: gcpPollInterval, MaxAttempts: gcpPollAttempts,
	}
}

// gcpStampSuffix makes an RFC3339 stamp safe for a GCP resource name (lowercase, hyphens).
func gcpStampSuffix(stamp string) string {
	r := make([]byte, 0, len(stamp))
	for i := 0; i < len(stamp); i++ {
		c := stamp[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z':
			r = append(r, c)
		case c >= 'A' && c <= 'Z':
			r = append(r, c+32)
		default:
			r = append(r, '-')
		}
	}
	return string(r)
}
