package secrets

import (
	"errors"

	"github.com/stackql-labs/omnisdk/internal/secretstore"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type Store = facade.SecretStore

var ErrNotFound = secretstore.ErrNotFound
var ErrUnavailable = secretstore.ErrUnavailable

type Config struct {
	Backend   string `json:"backend,omitempty"`
	Path      string `json:"path,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

func New(config Config) (Store, error) {
	switch config.Backend {
	case "", "file":
		if config.Namespace != "" {
			return nil, errors.New("namespace requires the OS backend")
		}
		return secretstore.NewFile(config.Path)
	case "os":
		if config.Path != "" {
			return nil, errors.New("OS backend accepts no file path")
		}
		if config.Namespace == "" {
			return nil, errors.New("OS secret store namespace is required")
		}
		return secretstore.NewOS(config.Namespace)
	default:
		return nil, errors.New("unknown secret store backend")
	}
}
