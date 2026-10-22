package namespace_test

import (
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/namespace"
)

func TestDeclaredWireNamesAndAliases(t *testing.T) {
	names := namespace.NewNaming("pascal", true)
	for wire, surface := range map[string]string{"VpcId": "vpc_id", "HTTPServer": "http_server", "openai-organization": "openai_organization", "already_snake": "already_snake"} {
		if got := names.Surface(wire); got != surface {
			t.Errorf("Surface(%q) = %q, want %q", wire, got, surface)
		}
		if got, err := names.Native(surface, []string{wire}); err != nil || got != wire {
			t.Errorf("Native(%q) = %q, %v", surface, got, err)
		}
	}
	if _, err := names.Native("vpc_id", []string{"VpcId", "VpcID"}); err == nil {
		t.Fatal("ambiguous native name accepted")
	}
	if got, err := namespace.NewNaming("", true).Native("vpc_id", []string{"VpcId"}); err != nil || got != "vpc_id" {
		t.Fatalf("undeclared native casing changed input: %q, %v", got, err)
	}
	if got := namespace.NewNaming("pascal", false).Surface("VpcId"); got != "VpcId" {
		t.Fatalf("provider without aliases changed response: %s", got)
	}
}
