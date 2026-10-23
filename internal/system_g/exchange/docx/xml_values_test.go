package docx

import (
	"reflect"
	"testing"
)

func TestXMLValuesNormalizeOnlyDeclaredScalarText(t *testing.T) {
	schema := requestShape(t).BodySchema()
	row := map[string]any{
		"Label":     map[string]any{"#text": "ap-southeast-1", "-xmlns": "urn:example"},
		"Options":   map[string]any{"Retries": map[string]any{"#text": "3"}, "-xmlns": "urn:options"},
		"Protocols": []any{map[string]any{"#text": "sftp"}, "ftp"},
		"Unknown":   map[string]any{"#text": "keep", "-xmlns": "urn:business"},
	}
	want := map[string]any{
		"Label":     "ap-southeast-1",
		"Options":   map[string]any{"Retries": "3", "-xmlns": "urn:options"},
		"Protocols": []any{"sftp", "ftp"},
		"Unknown":   map[string]any{"#text": "keep", "-xmlns": "urn:business"},
	}
	if got := xmlSchemaValue(row, schema); !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
	if _, ok := row["Label"].(map[string]any); !ok {
		t.Fatal("changed the source document")
	}
	label, _ := schema.Property("Label")
	business := map[string]any{"Value": "kept"}
	if got := xmlSchemaValue(business, label); !reflect.DeepEqual(got, business) {
		t.Fatalf("unwrapped business fields: %#v", got)
	}
	if got := xmlSchemaValue(map[string]any{"-xmlns": "urn:empty"}, label); got != nil {
		t.Fatalf("empty scalar=%#v", got)
	}
}
