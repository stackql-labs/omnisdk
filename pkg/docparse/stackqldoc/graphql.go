package stackqldoc

import (
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/jsonpath"
	"gopkg.in/yaml.v3"
)

type graphQLSpec struct {
	URL               string `yaml:"url"`
	HTTPVerb          string `yaml:"httpVerb"`
	Query             string `yaml:"query"`
	ResponseSelection struct {
		JSONPath string `yaml:"jsonPath"`
	} `yaml:"responseSelection"`
	Cursor struct {
		JSONPath            string `yaml:"jsonPath"`
		Strategy            string `yaml:"strategy"`
		Format              string `yaml:"format"`
		TerminateOnJSONPath string `yaml:"terminateOnJsonPath"`
		PageSize            int    `yaml:"pageSize"`
	} `yaml:"cursor"`
}

type graphQL struct{ spec *graphQLSpec }

func (g graphQL) URL() string             { return g.spec.URL }
func (g graphQL) HTTPVerb() string        { return g.spec.HTTPVerb }
func (g graphQL) Query() string           { return g.spec.Query }
func (g graphQL) Selection() string       { return g.spec.ResponseSelection.JSONPath }
func (g graphQL) CursorPath() string      { return g.spec.Cursor.JSONPath }
func (g graphQL) CursorStrategy() string  { return g.spec.Cursor.Strategy }
func (g graphQL) CursorFormat() string    { return g.spec.Cursor.Format }
func (g graphQL) TerminationPath() string { return g.spec.Cursor.TerminateOnJSONPath }
func (g graphQL) PageSize() int           { return g.spec.Cursor.PageSize }

// selectedSchema follows envelope fields without copying them. Only the row
// shape is materialised at the normal depth budget, including deeply nested
// GraphQL connections whose envelopes would exhaust that budget.
func selectedSchema(node *yaml.Node, components map[string]*yaml.Node, path string) aot.Schema {
	selection, err := jsonpath.Parse(path)
	if err != nil {
		return nil
	}
	for _, segment := range selection.Segments() {
		node = resolve(node, components)
		if segment.Array() {
			if item := schemaField(node, components, "items", 0); item != nil {
				node = item
			} else {
				if additional := schemaField(node, components, "additionalProperties", 0); additional != nil {
					node = additional
				} else {
					props := schemaField(node, components, "properties", 0)
					if props != nil && len(props.Content) == 2 {
						node = props.Content[1]
					} else {
						node = nil
					}
				}
			}
		} else {
			node = schemaProperty(node, components, segment.Name(), 0)
		}
		if node == nil {
			return nil
		}
	}
	node = resolve(node, components)
	if items := schemaField(node, components, "items", 0); items != nil {
		node = items
	}
	return newSchema(node, components)
}
