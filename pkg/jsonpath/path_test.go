package jsonpath_test

import (
	"github.com/stackql-labs/omnisdk/pkg/jsonpath"
	"reflect"
	"testing"
)

func TestSelectionsPreserveCardinalityAndCursorSlices(t *testing.T) {
	doc := map[string]any{"data": map[string]any{"edges": []any{map[string]any{"node": map[string]any{"name": "first"}, "cursor": "c1"}, map[string]any{"node": map[string]any{"name": "second"}, "cursor": "c2"}}}, "@odata.nextLink": "next"}
	for path, want := range map[string][]any{"$.data.edges[*].node.name": {"first", "second"}, "$.data.edges[-1:].cursor": {"c2"}, "$.data.edges[0].cursor": {"c1"}, "$.data.edges[7].cursor": {}, "$.data.edges[-9:1].cursor": {"c1"}, "$['@odata.nextLink']": {"next"}, "$.missing": {}} {
		selection, err := jsonpath.Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := selection.Values(doc); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
	for _, path := range []string{"$.data[?(@.x)]", "$.data[", "$.data[1:2:3]", "$.data."} {
		if _, err := jsonpath.Parse(path); err == nil {
			t.Errorf("unsupported path accepted: %s", path)
		}
	}
}
