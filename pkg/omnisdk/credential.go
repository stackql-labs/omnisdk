package omnisdk

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/auth"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Credential exposes the SDK authentication module to host authentication commands.
type Credential = facade.Credential

type credential struct {
	provider string
	config   Auth
}

// NewCredential validates only the explicitly configured provider's credential source.
func NewCredential(provider string, config Auth) Credential {
	return &credential{provider: provider, config: config}
}

func (c *credential) args() Args {
	return (Args{AuthByProvider: map[string]*Auth{c.provider: &c.config}}).withCredential(c.provider)
}

func (c *credential) Check() error {
	switch strings.ToLower(c.config.Type) {
	case "service_account":
		_, err := gcpCreds(c.args())
		return err
	case "aws_signing_v4", "aws_assume_role":
		_, err := awsCreds(c.args())
		return err
	default:
		_, err := auth.New(c.config.internal())
		return err
	}
}

func (c *credential) Principal() (string, error) {
	if strings.EqualFold(c.config.Type, "service_account") {
		creds, err := gcpCreds(c.args())
		if err != nil {
			return "", err
		}
		return creds.ClientEmail, nil
	}
	if err := c.Check(); err != nil {
		return "", err
	}
	if c.config.Username != "" {
		return c.config.Username, nil
	}
	return "", fmt.Errorf("auth: %s does not expose a principal", c.config.Type)
}

// Revoke revokes an interactive credential using its provider's authentication tool.
func (c *credential) Revoke(ctx context.Context) error {
	switch strings.ToLower(c.config.Type) {
	case "interactive":
		return exec.CommandContext(ctx, "gcloud", "auth", "revoke").Run()
	case "service_account":
		return fmt.Errorf("service account credentials cannot be revoked through AUTH REVOKE")
	default:
		return fmt.Errorf("auth revoke does not support method %q", c.config.Type)
	}
}
