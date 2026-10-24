package facade

import "context"

// Credential validates a configured credential source without executing a provider request.
type Credential interface {
	Check() error
	Revoke(context.Context) error
	Principal() (string, error)
}
