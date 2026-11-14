package omnisdk

import (
	"context"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func TestRoleSourcesAreSessionScopedAndRotateWithAuth(t *testing.T) {
	config := Auth{Type: "aws_assume_role", AccessKeyID: "base", SecretAccessKey: "secret", AssumeRole: &AWSAssumeRole{RoleARN: "arn:aws:iam::123456789012:role/example", STSRegion: "us-east-1"}}
	base := Args{AuthByProvider: map[string]*Auth{"aws": &config}}
	first, err := NewClientSession(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewClientSession(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	source := func(session ClientSession) facade.AWSCredentialSource {
		t.Helper()
		args, err := session.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		credentials, err := awsCreds(args.withCredential("aws"))
		if err != nil {
			t.Fatal(err)
		}
		return credentials.Source
	}
	original := source(first)
	if original == nil || original != source(first) {
		t.Fatal("session snapshots must reuse their role source")
	}
	if original == source(second) {
		t.Fatal("independent sessions must not share role credentials")
	}
	args, err := first.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	changed := config
	changed.SecretAccessKey = "rotated"
	args.AuthByProvider["aws"] = &changed
	rotated, err := awsCreds(args.withCredential("aws"))
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Source == original {
		t.Fatal("changed credentials must not reuse old source")
	}
}

func TestRoleConfigurationRejectsInvalidInputsWithoutNetwork(t *testing.T) {
	for _, role := range []AWSAssumeRole{
		{RoleARN: "invalid", STSRegion: "us-east-1"},
		{RoleARN: "arn:aws:iam::123456789012:role/example"},
		{RoleARN: "arn:aws:iam::123456789012:role/example", STSRegion: "us-east-1", DurationSeconds: 1},
		{RoleARN: "arn:aws:iam::123456789012:role/example", STSRegion: "us-east-1", Source: &Auth{Type: "aws_assume_role"}},
	} {
		config := Auth{Type: "aws_assume_role", AssumeRole: &role}
		_, err := awsCreds((Args{AuthByProvider: map[string]*Auth{"aws": &config}}).withCredential("aws"))
		if err == nil {
			t.Fatal("invalid role configuration accepted")
		}
	}
}
