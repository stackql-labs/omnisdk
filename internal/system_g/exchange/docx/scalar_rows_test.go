package docx

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/jsonpath"
	"reflect"
	"testing"
)

func TestDeclaredScalarXMLRowsRetainTheirColumn(t *testing.T) {
	selection := items{steps: []string{"Response", "Names", "member"}, scalarColumn: "member"}
	for _, input := range []any{"one", []any{"one", "two"}, nil} {
		output, err := selection.Apply(bind.NewDocRecord(map[string]any{"Response": map[string]any{"Names": map[string]any{"member": input}}}))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := output.Doc(facade.AnonymousPayload)
		want := []any{}
		switch v := input.(type) {
		case string:
			want = append(want, map[string]any{"member": v})
		case []any:
			for _, item := range v {
				want = append(want, map[string]any{"member": item})
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("input=%#v got=%#v want=%#v", input, got, want)
		}
	}
}

func TestStrictSelectionPreservesDefaultEmptyReadBehavior(t *testing.T) {
	for _, strict := range []bool{false, true} {
		selection := items{steps: []string{"items"}, strict: strict}
		_, err := selection.Apply(bind.NewDocRecord(map[string]any{}))
		if (err != nil) != strict {
			t.Fatalf("strict=%v error=%v", strict, err)
		}
		if err != nil && err.Error() != "error processing response: unknown key items" {
			t.Fatal(err)
		}
	}
}

func TestStrictJSONPathSelectionAcceptsAnEmptyDeclaredArray(t *testing.T) {
	path, err := jsonpath.Parse("$.data.items[*]")
	if err != nil {
		t.Fatal(err)
	}
	selection := selectionRows{selection: path, strict: true}
	for _, present := range []bool{false, true} {
		data := map[string]any{}
		if present {
			data["items"] = []any{}
		}
		_, err = selection.Apply(bind.NewDocRecord(map[string]any{"data": data}))
		if (err == nil) != present {
			t.Fatalf("present=%v error=%v", present, err)
		}
	}
}
