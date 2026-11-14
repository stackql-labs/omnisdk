package omnisdk

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/auth"
	"github.com/stackql-labs/omnisdk/internal/system_g/awssts"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/sdk"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/secret"
)

// AWSAssumeRole is explicit role selection, independent of downstream request scope.
// Source overrides the outer Auth's static keys/profile when supplied. No implicit role chaining.
type AWSAssumeRole struct {
	RoleARN          string `json:"role_arn"`
	RoleARNEnvVar    string `json:"role_arn_env_var,omitempty"`
	ExternalID       string `json:"external_id,omitempty"`
	ExternalIDEnvVar string `json:"external_id_env_var,omitempty"`
	SessionName      string `json:"session_name,omitempty"`
	DurationSeconds  int32  `json:"duration_seconds,omitempty"`
	STSRegion        string `json:"sts_region"`
	STSEndpoint      string `json:"sts_endpoint,omitempty"`
	Source           *Auth  `json:"source,omitempty"`
}

func awsRoleCreds(args Args) (sdk.Credentials, error) {
	configured := authOf(args)
	configured.Type = strings.ToLower(configured.Type)
	role := configured.AssumeRole
	if role == nil {
		return sdk.Credentials{}, fmt.Errorf("aws_assume_role requires explicit assume_role configuration")
	}
	if configured.Type != "" && configured.Type != "aws_assume_role" && configured.Type != "aws_signing_v4" {
		return sdk.Credentials{}, fmt.Errorf("assume_role cannot be combined with another authentication type")
	}
	arn, err := secret.Require("AWS role ARN", secret.Literal(role.RoleARN), secret.Env(role.RoleARNEnvVar)).Resolve()
	if err != nil {
		return sdk.Credentials{}, err
	}
	config := awssts.Config{RoleARN: arn, ExternalID: secret.Optional(secret.Literal(role.ExternalID), secret.Env(role.ExternalIDEnvVar)), SessionName: orStr(role.SessionName, "omnisdk"), DurationSeconds: role.DurationSeconds, Region: role.STSRegion, Endpoint: role.STSEndpoint}
	base := configured
	if role.Source != nil {
		base = *role.Source
		base.Type = strings.ToLower(base.Type)
		if base.AssumeRole != nil || base.Type == "aws_assume_role" {
			return sdk.Credentials{}, fmt.Errorf("nested AssumeRole sources are not supported")
		}
	}
	if base.Type != "" && base.Type != "aws_signing_v4" && base.Type != "aws_assume_role" {
		return sdk.Credentials{}, fmt.Errorf("STS source must provide static keys or an AWS profile")
	}
	sourceConfig := auth.AWSKeySourceConfig{
		AccessKeyID:     secret.Optional(secret.Literal(base.AccessKeyID), secret.Env(orStr(base.AccessKeyIDEnvVar, "AWS_ACCESS_KEY_ID"))),
		SecretAccessKey: secret.Optional(secret.Literal(base.SecretAccessKey), secret.Env(orStr(base.SecretAccessKeyEnvVar, "AWS_SECRET_ACCESS_KEY"))),
		SessionToken:    secret.Optional(secret.Literal(base.SessionToken), secret.Env(orStr(base.SessionTokenEnvVar, "AWS_SESSION_TOKEN"))),
		Profile:         orStr(base.Profile, os.Getenv("AWS_PROFILE")),
	}
	raw, err := json.Marshal(struct {
		Role   awssts.Config
		Source auth.AWSKeySourceConfig
	}{config, sourceConfig})
	if err != nil {
		return sdk.Credentials{}, fmt.Errorf("invalid STS configuration")
	}
	cache := args.awsRoleSources
	if cache == nil {
		cache = awssts.NewCache()
	}
	scope := args.awsCredentialScope
	if scope == "" {
		scope = "aws"
	}
	source, err := cache.Source(scope, sha256.Sum256(raw), func() (facade.AWSCredentialSource, error) {
		return awssts.New(config, auth.NewAWSKeySource(sourceConfig), nil)
	})
	if err != nil {
		return sdk.Credentials{}, err
	}
	return sdk.Credentials{Source: source}, nil
}

func awsRoleConfigured(args Args) bool {
	auth := authOf(args)
	return auth.AssumeRole != nil || strings.EqualFold(auth.Type, "aws_assume_role")
}

func withAWSRoleCache(args Args) Args {
	if args.awsRoleSources == nil {
		args.awsRoleSources = awssts.NewCache()
	}
	return args
}
