package docx

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/namespace"
	"testing"
)

func TestResponseRegionAliasDoesNotCollideWithRequestScope(t *testing.T) {
	names := rowNames{naming: namespace.NewNaming("PascalCase", true), scope: map[string]any{"region": "us-east-1", "AccessKeyId": "key-1"}}
	result, err := names.Apply(bind.NewDocRecord(map[string]any{"Region": "eu-west-1", "LastUsedDate": "2026-10-25"}))
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := result.Doc(facade.AnonymousPayload)
	row := doc.(map[string]any)
	for key, want := range map[string]any{"Region": "eu-west-1", "region": "us-east-1", "last_used_date": "2026-10-25", "AccessKeyId": "key-1", "access_key_id": "key-1"} {
		if row[key] != want {
			t.Fatalf("%s = %v, want %v", key, row[key], want)
		}
	}
}

func TestActualResponseAliasCollisionStillRejected(t *testing.T) {
	names := rowNames{naming: namespace.NewNaming("PascalCase", true)}
	if _, err := names.Apply(bind.NewDocRecord(map[string]any{"Region": "a", "region": "b"})); err == nil {
		t.Fatal("accepted ambiguous provider fields")
	}
}
