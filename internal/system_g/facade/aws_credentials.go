package facade

import (
	"context"
	"net/http"
	"time"
)

// AWSCredentials is an ephemeral credential value, never ledger or journal data.
type AWSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// AWSCredentialSource acquires credentials under the caller's cancellation scope.
type AWSCredentialSource interface {
	Retrieve(context.Context) (AWSCredentials, error)
}

// AWSCredentialCache retains only the current source generation for each session scope.
// Factories configure a source; they must not perform network IO.
type AWSCredentialCache interface {
	Source(string, [32]byte, func() (AWSCredentialSource, error)) (AWSCredentialSource, error)
}

// RequestAuthenticator runs at HTTP dispatch, outside pure transforms, on every attempt.
type RequestAuthenticator interface {
	Authenticate(context.Context, *http.Request, []byte) error
}
