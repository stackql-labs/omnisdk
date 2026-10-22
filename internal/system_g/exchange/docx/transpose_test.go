package docx

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"strings"
	"testing"
)

func TestAWSCanonicalDecodesStructuredSQLStrings(t *testing.T) {
	req, bound := transposed(httpx.Request{Query: map[string]string{"TagSpecification.1": "{TagSpecification}", "InstanceId.1": "{InstanceId}"}}, map[string]any{
		"TagSpecification": `[{"ResourceType":"volume","Tag":[{"Key":"id","Value":12345678901234567890}]}]`,
		"InstanceId":       `["id-001","id-002"]`,
	})
	want := map[string]string{"TagSpecification.1.ResourceType": "volume", "TagSpecification.1.Tag.1.Key": "id", "TagSpecification.1.Tag.1.Value": "12345678901234567890", "InstanceId.1": "id-001", "InstanceId.2": "id-002"}
	if len(req.Query) != len(want) {
		t.Fatalf("query = %v", req.Query)
	}
	for key, value := range want {
		slot := strings.TrimSuffix(strings.TrimPrefix(req.Query[key], "{"), "}")
		if bound[slot] != value {
			t.Errorf("%s = %v, want %s", key, bound[slot], value)
		}
	}
}
