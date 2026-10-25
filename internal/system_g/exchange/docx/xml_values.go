package docx

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// XML attributes describe wire encoding; a declared scalar remains a scalar.
// Objects and undeclared fields retain their document shape.
type xmlRowValues struct{ schema aot.Schema }

func (t xmlRowValues) Apply(in facade.Page) (facade.Record, error) {
	doc, ok := in.Doc(facade.AnonymousPayload)
	if !ok {
		return nil, nil
	}
	row, ok := xmlSchemaValue(doc, t.schema).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("XML row is not an object")
	}
	return bind.NewDocRecord(row), nil
}

func xmlSchemaValue(value any, schema aot.Schema) any {
	if schema == nil {
		return value
	}
	if typed, ok := schema.(aot.TypedSchema); ok && typed.Type() != "" && typed.Type() != "object" && typed.Type() != "array" {
		if object, ok := value.(map[string]any); ok {
			for key := range object {
				if key != "#text" && !strings.HasPrefix(key, "-") {
					return value
				}
			}
			value = object["#text"]
		}
		if text, ok := value.(string); ok && (typed.Type() == "integer" || typed.Type() == "number") {
			var number json.Number
			if len(text) > 0 && (text[0] == '-' || text[0] >= '0' && text[0] <= '9') && json.Unmarshal([]byte(text), &number) == nil {
				return number
			}
		}
		return value
	}
	switch values := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(values))
		for key, child := range values {
			declaration, _ := schema.Property(key)
			out[key] = xmlSchemaValue(child, declaration)
		}
		// Decode XML element names into the document's logical property names.
		// Read from the source so renaming cannot consume another property's value.
		for _, name := range schema.Properties() {
			property, ok := schema.Property(name)
			if !ok || property.WireName() == "" || property.WireName() == name {
				continue
			}
			wire := property.WireName()
			if child, present := values[wire]; present {
				delete(out, wire)
				out[name] = xmlSchemaValue(child, property)
			}
		}
		return out
	case []any:
		item, ok := schema.Items()
		if !ok {
			return value
		}
		out := make([]any, len(values))
		for i, child := range values {
			out[i] = xmlSchemaValue(child, item)
		}
		return out
	}
	return value
}

func declaredXMLRows(rows facade.Operator, response aot.Response, hasProgram bool) facade.Operator {
	if hasProgram || !strings.Contains(strings.ToLower(response.MediaType()), "xml") {
		return rows
	}
	schema := response.Schema()
	if selected, ok := response.(aot.SelectedSchema); ok && selected.RowSchema() != nil {
		schema = selected.RowSchema()
	}
	if schema == nil {
		return rows
	}
	if item, ok := schema.Items(); ok {
		schema = item
	}
	return exchange.NewTransformExchange(0, rows, xmlRowValues{schema: schema}, 1)
}
