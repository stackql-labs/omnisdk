package docx

import (
	"encoding/json"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
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
		"Options":   map[string]any{"Retries": json.Number("3"), "-xmlns": "urn:options"},
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

// The logical SQL name may differ from the XML element, including allOf annotations.
type renamedXMLSchema struct {
	aot.Schema
	wire string
}

func (s renamedXMLSchema) Type() string     { return "string" }
func (s renamedXMLSchema) Format() string   { return "" }
func (s renamedXMLSchema) WireName() string { return s.wire }

type renamedXMLRow struct{ aot.Schema }

func (s renamedXMLRow) Type() string         { return "object" }
func (s renamedXMLRow) Properties() []string { return []string{"AvailabilityZone"} }
func (s renamedXMLRow) Property(name string) (aot.Schema, bool) {
	if name == "AvailabilityZone" {
		return renamedXMLSchema{Schema: s.Schema, wire: "availabilityZone"}, true
	}
	return nil, false
}
func TestXMLValuesUseDeclaredWireNames(t *testing.T) {
	scalar, _ := requestShape(t).BodySchema().Property("Label")
	schema := renamedXMLRow{Schema: scalar}
	source := map[string]any{"availabilityZone": map[string]any{"#text": "east-a"}, "unknown": "kept"}
	want := map[string]any{"AvailabilityZone": "east-a", "unknown": "kept"}
	if got := xmlSchemaValue(source, schema); !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
	if _, ok := source["availabilityZone"]; !ok {
		t.Fatal("mutated input")
	}
}
