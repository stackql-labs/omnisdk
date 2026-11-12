package omnisdk_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func schemaRegistry(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, data := range map[string]string{"provider.yaml": "name: demo\nproviderServices:\n  service:\n    service: {$ref: service.yaml}\n", "service.yaml": schemaDocument} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const schemaDocument = `openapi: 3.0.0
servers:
  - url: https://example.test/{region}
    variables: {region: {default: east}}
paths:
  /json:
    post:
      operationId: createJson
      parameters: [{name: alt, in: query, schema: {type: string}}]
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [flag, settings]
              properties:
                flag: {type: boolean, description: Enables the feature}
                settings: {$ref: '#/components/schemas/Node'}
      responses:
        '200':
          content:
            application/json:
              schema: {type: object, properties: {items: {type: array, items: {$ref: '#/components/schemas/Node'}}}}
  /xml:
    put:
      operationId: putXml
      parameters: [{name: alt, in: query, schema: {type: string}}]
      requestBody:
        content:
          application/xml:
            schema:
              type: object
              properties:
                LifecycleConfiguration: {$ref: '#/components/schemas/Lifecycle'}
      responses: {'204': {description: accepted}}
    get:
      operationId: getXml
      parameters: [{name: alt, in: query, schema: {type: string}}]
      responses:
        '200':
          content:
            application/xml:
              schema: {$ref: '#/components/schemas/Lifecycle'}
components:
  schemas:
    Node:
      type: object
      required: [value]
      properties:
        value: {type: string, format: hostname, description: Node mode, enum: [small, large]}
        children: {type: array, items: {$ref: '#/components/schemas/Node'}}
    Lifecycle:
      type: object
      properties:
        Rules:
          type: array
          xml: {name: RuleList}
          items:
            type: object
            properties:
              Status: {type: string, enum: [Enabled, Disabled], description: Rule status}
              Filter: {type: object, properties: {Prefix: {type: string}}}
    Display:
      type: object
      properties:
        line_items: {type: array, items: {$ref: '#/components/schemas/Display'}}
  x-stackQL-resources:
    examples:
      methods:
        create:
          operation: {$ref: '#/paths/~1json/post'}
          response: {mediaType: application/json, objectKey: '$.items'}
        put:
          operation: {$ref: '#/paths/~1xml/put'}
          response: {openAPIDocKey: '204'}
        get:
          operation: {$ref: '#/paths/~1xml/get'}
          response:
            mediaType: application/xml
            overrideMediaType: application/json
            objectKey: '$.line_items'
            schema_override: {$ref: '#/components/schemas/Display'}
            transform: {type: schema_driven_xml_v0.1.0}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/examples/methods/get'}]
        insert: [{$ref: '#/components/x-stackQL-resources/examples/methods/create'}]
`

func fieldNamed(t *testing.T, fields []omnisdk.FieldDescription, name string) omnisdk.FieldDescription {
	t.Helper()
	for _, f := range fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("field %q missing: %v", name, fields)
	return omnisdk.FieldDescription{}
}
func TestMethodSchemaJSONDepthPathAndRecursiveReferences(t *testing.T) {
	root := schemaRegistry(t)
	address := "stackql_unstable_demo.service.examples"
	fields, err := omnisdk.DescribeMethodFields(root, address, "create", omnisdk.MethodSchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	flag := fieldNamed(t, fields, "data__flag")
	if flag.Type != "boolean" || !flag.Required || flag.Description != "" {
		t.Fatal(flag)
	}
	settings := fieldNamed(t, fields, "data__settings")
	if settings.Type != "object" || settings.Children != 2 || !settings.Truncated || len(settings.Properties) != 0 {
		t.Fatal(settings)
	}
	methods, err := omnisdk.DocMethods(root, "stackql_unstable_demo", "service", "examples")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range methods {
		if method.Name == "create" {
			for _, field := range fields {
				if strings.HasPrefix(field.ParamType, "input_") && field.Required != slices.Contains(method.RequiredParams, field.Name) {
					t.Fatalf("listing/description required flags disagree: method=%v field=%v", method, field)
				}
			}
		}
	}
	region := fieldNamed(t, fields, "region")
	if !region.Required {
		t.Fatal("region scope not required", region)
	}
	for _, f := range fields {
		if f.Name == "alt" {
			t.Fatal("global query control included by default")
		}
	}
	encoded, _ := json.Marshal(fields)
	if len(encoded) > 4096 {
		t.Fatalf("shallow view unbounded: %d", len(encoded))
	}
	view, err := omnisdk.DescribeMethodFields(root, address, "create", omnisdk.MethodSchemaOptions{Path: "data__settings.children.value", Descriptions: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(view) != 1 || view[0].Type != "string" || view[0].Format != "hostname" || view[0].Description != "Node mode" || len(view[0].Enum) != 2 {
		t.Fatal(view)
	}
	signature, err := omnisdk.DescribeMethod(root, address, "create")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := signature.(omnisdk.SchemaMethodSignature).BodySchema()
	if !ok {
		t.Fatal("missing body")
	}
	var node omnisdk.FieldSchema
	for _, f := range body.Properties() {
		if f.Name() == "settings" {
			node = f
		}
	}
	for range 12 {
		var child omnisdk.FieldSchema
		for _, f := range node.Properties() {
			if f.Name() == "children" {
				child = f
			}
		}
		node, ok = child.Items()
		if !ok || len(node.Properties()) != 2 {
			t.Fatal("recursive reference was truncated")
		}
	}
	// Returned metadata slices cannot mutate subsequent discovery.
	for _, f := range node.Properties() {
		if f.Name() == "value" {
			values := f.Enum()
			values[0] = "corrupt"
			if f.Enum()[0] == "corrupt" {
				t.Fatal("enum mutation escaped")
			}
		}
	}
	all, err := omnisdk.DescribeMethodFields(root, address, "create", omnisdk.MethodSchemaOptions{IncludeGlobal: true, ParamType: "input"})
	if err != nil {
		t.Fatal(err)
	}
	fieldNamed(t, all, "alt")
	for _, options := range []omnisdk.MethodSchemaOptions{{Depth: -1}, {Depth: 33}, {Path: "data__settings.nope"}, {ParamType: "invalid"}} {
		_, err := omnisdk.DescribeMethodFields(root, address, "create", options)
		if err == nil {
			t.Fatalf("invalid or excessive view accepted: %+v", options)
		}
	}
}
func TestMethodSchemaXMLRequestAndTransformedRows(t *testing.T) {
	root := schemaRegistry(t)
	address := "stackql_unstable_demo.service.examples"
	fields, err := omnisdk.DescribeMethodFields(root, address, "put", omnisdk.MethodSchemaOptions{Path: "data__LifecycleConfiguration.Rules", Depth: 3, Descriptions: true})
	if err != nil {
		t.Fatal(err)
	}
	rules := fields[0]
	if rules.Type != "array" || rules.WireName != "RuleList" || rules.Items == nil {
		t.Fatal(rules)
	}
	status := fieldNamed(t, rules.Items.Properties, "Status")
	if len(status.Enum) != 2 || status.Description != "Rule status" {
		t.Fatal(status)
	}
	outputs, err := omnisdk.DescribeMethodFields(root, address, "get", omnisdk.MethodSchemaOptions{ParamType: "output"})
	if err != nil {
		t.Fatal(err)
	}
	fieldNamed(t, outputs, "Rules")
	for _, f := range outputs {
		if f.Name == "line_items" {
			t.Fatal("exposed transform envelope instead of row")
		}
	}
}
func TestMethodSchemaIssue39PublishedContracts(t *testing.T) {
	requireCorpus(t)
	defaultView, err := omnisdk.DescribeMethodFields(corpus, "stackql_unstable_google.compute.instances", "insert", omnisdk.MethodSchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(defaultView)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 64<<10 {
		t.Fatalf("default discovery exceeds 64 KiB: %d", len(encoded))
	}
	for _, f := range defaultView {
		if f.Name == "alt" || f.Name == "oauth_token" {
			t.Fatalf("default discovery included global parameter %q", f.Name)
		}
	}
	for _, tc := range []struct{ address, method, path, typ string }{
		{"stackql_unstable_google.compute.instances", "insert", "data__canIpForward", "boolean"},
		{"stackql_unstable_google.compute.instances", "insert", "data__networkInterfaces", "array"},
		{"stackql_unstable_aws.s3.bucket_lifecycle_configurations", "put_bucket_lifecycle_configuration", "data__LifecycleConfiguration", "object"},
	} {
		fields, err := omnisdk.DescribeMethodFields(corpus, tc.address, tc.method, omnisdk.MethodSchemaOptions{Path: tc.path})
		if err != nil {
			t.Fatal(err)
		}
		if len(fields) != 1 || fields[0].Type != tc.typ {
			t.Fatalf("%s %s: %v", tc.address, tc.path, fields)
		}
	}
	inputs, err := omnisdk.DescribeMethodFields(corpus, "stackql_unstable_aws.s3.bucket_lifecycle_configurations", "get_bucket_lifecycle_configuration", omnisdk.MethodSchemaOptions{ParamType: "input"})
	if err != nil {
		t.Fatal(err)
	}
	if !fieldNamed(t, inputs, "region").Required {
		t.Fatal("published XML method lost required region scope")
	}
	_, err = omnisdk.DescribeMethodFields(corpus, "stackql_unstable_aws.s3.bucket_lifecycle_configurations", "get_bucket_lifecycle_configuration", omnisdk.MethodSchemaOptions{ParamType: "output"})
	if err == nil || !strings.Contains(err.Error(), "self-referencing response envelope") {
		t.Fatalf("malformed published schema not diagnosed: %v", err)
	}
}

func TestMethodSchemaRejectsExcessiveExpansion(t *testing.T) {
	root := schemaRegistry(t)
	path := filepath.Join(root, "service.yaml")
	document := strings.Replace(schemaDocument, "        children: {type: array, items: {$ref: '#/components/schemas/Node'}}", "        children: {type: array, items: {$ref: '#/components/schemas/Node'}}\n        sibling: {type: array, items: {$ref: '#/components/schemas/Node'}}", 1)
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := omnisdk.DescribeMethodFields(root, "stackql_unstable_demo.service.examples", "create", omnisdk.MethodSchemaOptions{Depth: 32, Path: "data__settings"})
	if err == nil || !strings.Contains(err.Error(), "4096 nodes") {
		t.Fatalf("unbounded recursive expansion accepted: %v", err)
	}
}

func TestMethodSchemaXMLRowsAgreeWithExecutedTransform(t *testing.T) {
	root := schemaRegistry(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<Lifecycle><RuleList><Status>Enabled</Status><Filter><Prefix>logs/</Prefix></Filter></RuleList></Lifecycle>`)
	}))
	defer server.Close()
	graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("rules", "stackql_unstable_demo.service.examples", map[string]string{"region": "east"})}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := omnisdk.NewGraphQuery(root, graph, omnisdk.Args{Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, plan)
	if len(rows) != 1 || rows[0]["Rules"] == nil || rows[0]["line_items"] != nil {
		t.Fatalf("execution disagrees with selected row schema: %v", rows)
	}
}

func TestMethodSchemaRejectsRecursiveArrayPath(t *testing.T) {
	root := schemaRegistry(t)
	document := strings.Replace(schemaDocument, "    Node:\n      type: object", "    Node:\n      type: array\n      items: {$ref: '#/components/schemas/Node'}", 1)
	if err := os.WriteFile(filepath.Join(root, "service.yaml"), []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := omnisdk.DescribeMethodFields(root, "stackql_unstable_demo.service.examples", "create", omnisdk.MethodSchemaOptions{Path: "data__settings.value"})
	if err == nil || !strings.Contains(err.Error(), "recursive array items") {
		t.Fatalf("recursive array traversal not bounded: %v", err)
	}
}
