package docx

import (
	"bytes"
	"encoding/xml"
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
	"io"
	"strings"
)

// Legacy XML tables expose each immediate row child's complete text content.
// Keeping this at decoding preserves whitespace and sibling order, both lost
// once XML has been reduced to an unordered JSON object.
type legacyXMLDecoder struct{ rowPath []string }
type xmlTextNode struct {
	name     string
	text     strings.Builder
	children []*xmlTextNode
}

func (d legacyXMLDecoder) Apply(page facade.Page) (facade.Record, error) {
	payload := page.Bytes(facade.AnonymousPayload)
	if len(payload) == 0 {
		return nil, nil
	}
	decoder := xml.NewDecoder(bytes.NewReader(payload))
	root := &xmlTextNode{}
	stack := []*xmlTextNode{root}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			node := &xmlTextNode{name: token.Name.Local}
			parent := stack[len(stack)-1]
			parent.children = append(parent.children, node)
			stack = append(stack, node)
		case xml.CharData:
			for _, node := range stack {
				node.text.Write(token)
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
	return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(d.object(root, nil))}), nil
}
func (d legacyXMLDecoder) object(node *xmlTextNode, path []string) any {
	if len(node.children) == 0 {
		return node.text.String()
	}
	row := len(path) == len(d.rowPath) && len(path) > 0
	for i := range path {
		if row && d.rowPath[i] != "*" && d.rowPath[i] != path[i] {
			row = false
		}
	}
	output := map[string]any{}
	for _, child := range node.children {
		childPath := append(append([]string(nil), path...), child.name)
		var value any
		if row {
			value = child.text.String()
		} else {
			value = d.object(child, childPath)
		}
		if old, exists := output[child.name]; exists {
			if list, ok := old.([]any); ok {
				output[child.name] = append(list, value)
			} else {
				output[child.name] = []any{old, value}
			}
		} else {
			output[child.name] = value
		}
	}
	return output
}

type legacyBooleanValues struct{}

func (legacyBooleanValues) Apply(page facade.Page) (facade.Record, error) {
	doc, ok := page.Doc(facade.AnonymousPayload)
	if !ok {
		return nil, nil
	}
	row, ok := doc.(map[string]any)
	if !ok {
		return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(doc)}), nil
	}
	output := make(map[string]any, len(row))
	for key, value := range row {
		if flag, ok := value.(bool); ok {
			if flag {
				value = int64(1)
			} else {
				value = int64(0)
			}
		}
		output[key] = value
	}
	return bind.NewDocRecord(output), nil
}
