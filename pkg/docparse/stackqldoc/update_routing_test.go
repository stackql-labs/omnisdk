package stackqldoc_test

import (
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
	"strings"
	"testing"
	"testing/fstest"
)

const updateDocument = `openapi: 3.0.0
servers: [{url: 'https://example.test'}]
updates:
 - resources: {$ref: '#/components/x-stackQL-resources/items'}
   priority: 20
   permits: [name]
   target: '#/components/x-stackQL-resources/items/methods/second'
paths:
 /items:
  patch:
   operationId: changeItems
   responses: {'200': {content: {application/json: {schema: {type: object}}}}}
components:
 x-stackQL-resources:
  items:
   updates:
    - priority: 20
      requires: [name]
      permits: [name, tags]
      target: {$ref: '#/components/x-stackQL-resources/items/methods/first'}
   methods:
    first:
     updates:
      - priority: 30
        permits: [tags]
        target: '#/components/x-stackQL-resources/items/methods/first'
     operation: {$ref: '#/paths/~1items/patch'}
    second:
     operation: {$ref: '#/paths/~1items/patch'}
   sqlVerbs:
    update: [{$ref: '#/components/x-stackQL-resources/items/methods/first'}, {$ref: '#/components/x-stackQL-resources/items/methods/second'}]
`

func TestUpdateRoutesAcrossHierarchy(t *testing.T) {
	d, err := stackqldoc.Parse([]byte(updateDocument))
	if err != nil {
		t.Fatal(err)
	}
	routes, err := d.(aot.UpdateRouting).UpdateRoutes("items")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 || routes[0].Exchange().Name() != "second" || routes[1].Exchange().Name() != "first" || routes[2].Priority() != 30 {
		t.Fatalf("routes=%v", routes)
	}
	p := routes[1].Permits()
	p[0] = "corrupted"
	if routes[1].Permits()[0] != "name" {
		t.Fatal("mutable route metadata")
	}
}
func TestMalformedUpdateDeclarations(t *testing.T) {
	for name, data := range map[string]string{
		"unknown field":        strings.Replace(updateDocument, "priority: 20", "priority: 20\n   prioirty: 2", 1),
		"missing priority":     strings.Replace(updateDocument, "   priority: 20\n", "", 1),
		"missing permits":      strings.Replace(updateDocument, "   permits: [name]\n", "", 1),
		"duplicate attributes": strings.Replace(updateDocument, "permits: [name]", "permits: [name, name]", 1),
		"outside permits":      strings.Replace(updateDocument, "permits: [name]", "requires: [missing]\n   permits: [name]", 1),
		"unscoped":             strings.Replace(updateDocument, "resources: {$ref: '#/components/x-stackQL-resources/items'}", "requires: []", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := stackqldoc.Parse([]byte(data)); err == nil {
				t.Fatal("invalid update route accepted")
			}
		})
	}
	for _, replacement := range []string{"#/components/x-stackQL-resources/missing/methods/first", "#/components/x-stackQL-resources/items/methods/missing", "#/components/x-stackQL-resources/items", "https://outside.test/doc.yaml#/components/x-stackQL-resources/items/methods/first"} {
		data := strings.ReplaceAll(updateDocument, "#/components/x-stackQL-resources/items/methods/first", replacement)
		d, err := stackqldoc.Parse([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.(aot.UpdateRouting).UpdateRoutes("items"); err == nil {
			t.Fatalf("invalid target accepted: %s", replacement)
		}
	}
}
func TestProviderRoutesAndExternalTargets(t *testing.T) {
	provider := `name: demo
updates:
 - resources: {$ref: 'service.yaml#/components/x-stackQL-resources/items'}
   priority: 99
   permits: [name]
   target: {$ref: 'service.yaml#/components/x-stackQL-resources/items/methods/first'}
providerServices:
 service:
  service: {$ref: service.yaml}
`
	cat, err := stackqldoc.Open(fstest.MapFS{"provider.yaml": {Data: []byte(provider)}, "service.yaml": {Data: []byte(updateDocument)}})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := cat.(aot.UpdateRouting).UpdateRoutes("stackql_unstable_demo.service.items")
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 4 {
		t.Fatalf("routes=%d", len(routes))
	}
	r := routes[3]
	if r.Source() != "demo/provider.yaml" || r.TargetResource() != "stackql_unstable_demo.service.items" || r.Exchange().OperationID() != "changeItems" {
		t.Fatalf("route=%v", r)
	}
}
