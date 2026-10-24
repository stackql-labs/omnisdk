package docx

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

type xmlRequestBody struct {
	declaration aot.XMLRequest
	schema      aot.Schema
}

func (t xmlRequestBody) Apply(in facade.Page) (facade.Record, error) {
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(httpx.ReqBody(in)))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("XML request body: %w", err)
	}
	root := xml.StartElement{Name: xml.Name{Local: t.declaration.XMLRootName()}}
	if annotation := t.declaration.XMLRootAnnotation(); annotation != "" {
		attributes := xml.NewDecoder(strings.NewReader("<root " + annotation + "></root>"))
		token, err := attributes.Token()
		if err != nil {
			return nil, fmt.Errorf("XML request annotation: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "root" {
			return nil, fmt.Errorf("XML request annotation must declare attributes")
		}
		root.Attr = start.Attr
	}
	var output bytes.Buffer
	output.WriteString(t.declaration.XMLDeclaration())
	encoder := xml.NewEncoder(&output)
	if err := encoder.EncodeToken(root); err != nil {
		return nil, err
	}
	if err := writeXMLProperties(encoder, &output, body, t.schema); err != nil {
		return nil, err
	}
	if err := encoder.EncodeToken(root.End()); err != nil {
		return nil, err
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	return httpx.NewRequestRecord(httpx.Method(in), httpx.URL(in), httpx.Header(in), output.Bytes()), nil
}

func writeXMLProperties(encoder *xml.Encoder, output *bytes.Buffer, body map[string]any, schema aot.Schema) error {
	for _, name := range slices.Sorted(maps.Keys(body)) {
		var property aot.Schema
		if schema != nil {
			property, _ = schema.Property(name)
		}
		if err := writeXMLProperty(encoder, output, name, body[name], property); err != nil {
			return err
		}
	}
	return nil
}

func writeXMLProperty(encoder *xml.Encoder, output *bytes.Buffer, name string, value any, schema aot.Schema) error {
	if wire, ok := schema.(interface{ WireName() string }); ok && wire.WireName() != "" {
		name = wire.WireName()
	}
	if values, array := value.([]any); array {
		var item aot.Schema
		if schema != nil {
			item, _ = schema.Items()
		}
		// An explicit item element names a wrapped collection. Without it,
		// retain the repeated-property encoding used by ordinary XML bodies.
		if wire, named := item.(interface{ WireName() string }); named && wire.WireName() != "" {
			start := xml.StartElement{Name: xml.Name{Local: name}}
			if err := encoder.EncodeToken(start); err != nil {
				return err
			}
			for _, value := range values {
				if err := writeXMLProperty(encoder, output, wire.WireName(), value, item); err != nil {
					return err
				}
			}
			return encoder.EncodeToken(start.End())
		}
		for _, value := range values {
			if err := writeXMLProperty(encoder, output, name, value, item); err != nil {
				return err
			}
		}
		return nil
	}
	start := xml.StartElement{Name: xml.Name{Local: name}}
	if err := encoder.EncodeToken(start); err != nil {
		return err
	}
	switch value := value.(type) {
	case map[string]any:
		if err := writeXMLProperties(encoder, output, value, schema); err != nil {
			return err
		}
	case string:
		typed, structured := schema.(aot.TypedSchema)
		if structured && (typed.Type() == "object" || typed.Type() == "array") && strings.HasPrefix(strings.TrimSpace(value), "<") {
			if err := validateXMLFragment(value); err != nil {
				return err
			}
			if err := encoder.Flush(); err != nil {
				return err
			}
			output.WriteString(value)
		} else if err := encoder.EncodeToken(xml.CharData(value)); err != nil {
			return err
		}
	default:
		if value != nil {
			if err := encoder.EncodeToken(xml.CharData(fmt.Sprint(value))); err != nil {
				return err
			}
		}
	}
	return encoder.EncodeToken(start.End())
}

func validateXMLFragment(fragment string) error {
	decoder := xml.NewDecoder(strings.NewReader("<fragment>" + fragment + "</fragment>"))
	depth, roots := 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("invalid XML request fragment: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return fmt.Errorf("XML request fragment escapes its property")
				}
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.Directive, xml.ProcInst:
			return fmt.Errorf("XML request fragment cannot declare processing instructions or directives")
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(token)) != "" {
				return fmt.Errorf("XML request fragment escapes its property")
			}
		}
	}
}
