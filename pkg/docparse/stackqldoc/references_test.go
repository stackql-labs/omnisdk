package stackqldoc_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

func TestServiceNameCollisionCannotReplaceAnotherService(t *testing.T) {
	fsys := fstest.MapFS{
		"provider.yaml": {Data: []byte(`name: p
providerServices:
  ec2:
    name: ec2
    preferred: true
    service: {$ref: 'services/ec2.yaml'}
  iam:
    name: ec2
    preferred: true
    service: {$ref: 'services/iam.yaml'}
`)},
		"services/ec2.yaml": {Data: []byte("openapi: 3.0.0\ncomponents:\n  x-stackQL-resources:\n    volumes: {methods: {}}\n")},
		"services/iam.yaml": {Data: []byte("openapi: 3.0.0\ncomponents:\n  x-stackQL-resources:\n    users: {methods: {}}\n")},
	}
	for range 20 {
		catalog, err := stackqldoc.Open(fsys)
		if err != nil {
			t.Fatal(err)
		}
		for service, want := range map[string]string{"ec2": "volumes", "iam": "users"} {
			resources, err := catalog.Resources(service)
			if err != nil || len(resources) != 1 || resources[0] != want {
				t.Fatalf("%s resources = %v, %v", service, resources, err)
			}
		}
	}
}

func TestSplitResourceOperationsUseTheirOwnSchemas(t *testing.T) {
	fsys := fstest.MapFS{
		"provider.yaml": {Data: []byte(`id: p
name: p
providerServices:
  compute:
    name: compute
    resources: {$ref: 'p/v1/resources/compute.yaml'}
    service: {$ref: 'p/v1/services/absent.yaml'}
`)},
		"resources/compute.yaml": {Data: []byte(`resources:
  items:
    methods:
      list:
        operation: {$ref: 'p/v1/services-split/compute/main.yaml#/paths/~1items/GET'}
        response: {mediaType: application/json, objectKey: '$.items'}
    sqlVerbs:
      select: [{$ref: '#/resources/items/methods/list'}]
serviceDoc: {$ref: 'p/v1/services-split/compute/main.yaml'}
`)},
		"services-split/compute/main.yaml": {Data: []byte(`openapi: 3.0.0
servers: [{url: 'https://example.test'}]
paths:
  /items:
    get:
      operationId: listItems
      responses:
        '200':
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Envelope'}
components:
  schemas:
    Envelope:
      type: object
      properties:
        items:
          type: array
          items:
            type: object
            properties:
              id: {type: string}
`)},
	}
	cache := &countingCache{docs: map[string]stackqldoc.Doc{}}
	cat, err := stackqldoc.Open(fsys, stackqldoc.WithDocCache(cache, "split"))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := cat.Exchange("stackql_unstable_p.compute.items")
	if err != nil {
		t.Fatal(err)
	}
	if ex.OperationID() != "listItems" || ex.Request().URL() != "https://example.test/items" {
		t.Fatalf("wrong operation: %s %s", ex.OperationID(), ex.Request().URL())
	}
	items, ok := ex.Response().Schema().Property("items")
	if !ok {
		t.Fatal("referenced schema was not resolved")
	}
	row, ok := items.Items()
	if !ok {
		t.Fatal("missing row schema")
	}
	if _, ok := row.Property("id"); !ok {
		t.Fatal("missing id")
	}
	// The resource cache must not freeze a referenced service document.
	fsys["services-split/compute/main.yaml"].Data = []byte(strings.ReplaceAll(string(fsys["services-split/compute/main.yaml"].Data), "listItems", "listChanged"))
	ex, err = cat.Exchange("stackql_unstable_p.compute.items")
	if err != nil || ex.OperationID() != "listChanged" {
		t.Fatalf("cached external document is stale: %v %v", ex, err)
	}
}

func TestLegacyMethodMatchingAndExplicitDisable(t *testing.T) {
	for _, verbMap := range []string{"", "      sqlVerbs: {select: []}\n"} {
		d, err := stackqldoc.Parse([]byte(`openapi: 3.0.0
servers: [{url: 'https://example.test'}]
paths:
  /items:
    get: {operationId: listItems}
  /items/{id}:
    get: {operationId: getItem}
components:
  x-stackQL-resources:
    items:
      methods:
        get:
          operation: {$ref: '#/paths/~1items~1{id}/GET'}
        list:
          operation: {$ref: '#/paths/~1items/GET'}
` + verbMap))
		if err != nil {
			t.Fatal(err)
		}
		ops, err := d.Verb("items", "select")
		if verbMap != "" {
			if err == nil {
				t.Fatal("explicit disabled SELECT was inferred")
			}
			continue
		}
		if err != nil || len(ops) != 2 {
			t.Fatalf("legacy methods: %v %v", ops, err)
		}
		if ops[0].OperationID() != "listItems" || ops[1].OperationID() != "getItem" {
			t.Fatal("legacy priority not retained")
		}
		methods, err := d.Methods("items")
		if err != nil || methods[0].SQLVerb() != "select" || methods[1].SQLVerb() != "select" {
			t.Fatalf("method metadata: %v %v", methods, err)
		}
	}
}

func TestExternalOperationCannotEscapeBundle(t *testing.T) {
	fsys := fstest.MapFS{
		"provider.yaml": {Data: []byte("name: p\nproviderServices:\n  s:\n    name: s\n    service: {$ref: 'services/s.yaml'}\n")},
		"services/s.yaml": {Data: []byte(`components:
  x-stackQL-resources:
    r:
      methods:
        list: {operation: {$ref: '../../outside.yaml#/paths/~1/get'}}
`)},
	}
	cat, err := stackqldoc.Open(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cat.Exchange("stackql_unstable_p.s.r"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escaping reference: %v", err)
	}
}
