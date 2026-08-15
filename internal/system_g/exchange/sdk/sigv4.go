package sdk

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/awsv4"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// AWS SigV4 lives in its own package: it is a provider primitive, not part of this hand-authored
// catalog, and the document compiler needs it without depending on the catalog. These aliases keep it
// spelled the same at every existing call site.
type (
	// Credentials are AWS SigV4 credentials.
	Credentials = awsv4.Credentials
	// Signer signs a request.
	Signer = awsv4.Signer
)

// NewSigV4Signer builds a region/service-scoped signer.
func NewSigV4Signer(region, service string, creds Credentials, signPayloadHeader bool) Signer {
	return awsv4.NewSigV4Signer(region, service, creds, signPayloadHeader)
}

// NewSigV4Transform adapts a signer to the request-transform seam.
func NewSigV4Transform(s Signer) facade.Transform { return awsv4.NewSigV4Transform(s) }
