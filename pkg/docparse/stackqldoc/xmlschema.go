package stackqldoc

import (
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"gopkg.in/yaml.v3"
	"strings"
)

func schemaAtSelection(node *yaml.Node, components map[string]*yaml.Node, selection string) aot.Schema {
	if selection == "" {
		return newSchema(node, components)
	}
	if strings.HasPrefix(selection, "/") {
		return selectedXMLSchema(node, components, selection)
	}
	if !strings.HasPrefix(selection, "$") {
		selection = "$." + selection
	}
	return selectedSchema(node, components, selection)
}

// XML wire envelopes and repeated item/member elements need not be represented
// as explicit OpenAPI properties. Walk the raw schema before applying the normal
// row depth bound, so deeply nested response envelopes do not truncate rows.
func selectedXMLSchema(node *yaml.Node, components map[string]*yaml.Node, path string) aot.Schema {
	for _, segment := range strings.Split(path, "/") {
		if segment == "" {
			continue
		}
		if segment == "*" {
			if props := schemaField(node, components, "properties", 0); props != nil && len(props.Content) == 2 {
				name := props.Content[0].Value
				if strings.HasSuffix(name, "Response") || strings.HasSuffix(name, "Result") {
					node = props.Content[1]
				}
			}
			continue
		}
		if next := schemaProperty(node, components, segment, 0); next != nil {
			node = next
			continue
		}
		if segment == "item" || segment == "member" {
			if item := schemaField(node, components, "items", 0); item != nil {
				node = item
				continue
			}
		}
		if strings.HasSuffix(segment, "Result") || strings.HasSuffix(segment, "Response") {
			continue
		}
		return nil
	}
	if item := schemaField(node, components, "items", 0); item != nil {
		node = item
	}
	return newSchema(node, components)
}

func schemaField(node *yaml.Node, components map[string]*yaml.Node, name string, depth int) *yaml.Node {
	if depth > 16 {
		return nil
	}
	node = resolve(node, components)
	if found := field(node, name); found != nil {
		return found
	}
	if branches := field(node, "allOf"); branches != nil {
		for _, branch := range branches.Content {
			if found := schemaField(branch, components, name, depth+1); found != nil {
				return found
			}
		}
	}
	return nil
}
func schemaProperty(node *yaml.Node, components map[string]*yaml.Node, name string, depth int) *yaml.Node {
	if depth > 16 {
		return nil
	}
	node = resolve(node, components)
	if properties := field(node, "properties"); properties != nil {
		if found := field(properties, name); found != nil {
			return found
		}
		for i := 0; i+1 < len(properties.Content); i += 2 {
			if xmlName(properties.Content[i+1]) == name {
				return properties.Content[i+1]
			}
		}
	}
	if branches := field(node, "allOf"); branches != nil {
		for _, branch := range branches.Content {
			if found := schemaProperty(branch, components, name, depth+1); found != nil {
				return found
			}
		}
	}
	return nil
}
