package docx

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
	"testing"
)

func TestLegacyXMLRetainsNestedTextWhitespaceAndOrder(t *testing.T) {
	decoder := legacyXMLDecoder{rowPath: []string{"Response", "Records", "Record"}}
	input := record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewBytesValue([]byte("<Response><Records><Record><Name>test</Name><Values>\n  <Value>first</Value>\n  <Value>second</Value>\n</Values></Record></Records></Response>"))})
	decoded, err := decoder.Apply(input)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := (items{steps: decoder.rowPath}).Apply(decoded)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := selected.Doc(facade.AnonymousPayload)
	rows := doc.([]any)
	row := rows[0].(map[string]any)
	if row["Name"] != "test" || row["Values"] != "\n  first\n  second\n" {
		t.Fatalf("row=%#v", row)
	}
}
