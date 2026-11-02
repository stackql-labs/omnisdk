package facade

import "context"

type SecretStore interface {
	Get(context.Context, string) ([]byte, error)
	Set(context.Context, string, []byte) error
	Delete(context.Context, string) error
}

type SessionState interface {
	Set(string, []byte)
	Delete(string)
	Snapshot(...string) map[string][]byte
}
