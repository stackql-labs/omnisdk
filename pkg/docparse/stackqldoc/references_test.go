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

func TestOperationAndPathServersOverrideDocumentServer(t *testing.T) {
	for _, tc := range []struct{ name, root, path, operation, want, variable string }{
		{"document", "servers: [{url: 'https://document.test'}]\n", "", "", "https://document.test/items", ""},
		{"path", "servers: [{url: 'https://document.test'}]\n", "    servers: [{url: 'https://path.test', variables: {Bucket: {default: ignored}}}]\n", "", "https://path.test/items", "Bucket"},
		{"operation", "servers: [{url: 'https://document.test'}]\n", "    servers: [{url: 'https://path.test'}]\n", "      servers: [{url: 'https://operation.test'}]\n", "https://operation.test/items", ""},
		{"path without root", "", "    servers: [{url: 'https://path.test'}]\n", "", "https://path.test/items", ""},
		{"operation without root", "", "", "      servers: [{url: 'https://operation.test'}]\n", "https://operation.test/items", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := "openapi: 3.0.0\n" + tc.root + "paths:\n  /items:\n" + tc.path + "    get:\n" + tc.operation + `      responses:
        '200': {description: OK}
components:
  x-stackQL-resources:
    items:
      methods:
        list:
          operation: {$ref: '#/paths/~1items/get'}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/items/methods/list'}]
`
			doc, err := stackqldoc.Parse([]byte(source))
			if err != nil {
				t.Fatal(err)
			}
			exchange, err := doc.Select("items")
			if err != nil {
				t.Fatal(err)
			}
			if got := exchange.Request().URL(); got != tc.want {
				t.Fatalf("URL=%q want=%q", got, tc.want)
			}
			if tc.variable != "" {
				found := false
				for _, parameter := range exchange.Request().Parameters() {
					if parameter.Name() == tc.variable {
						found = true
					}
				}
				if !found {
					t.Fatalf("lost declared scope variable %s", tc.variable)
				}
			}
		})
	}
}

func TestLegacyPathAndVerbResolveTheSameOperation(t *testing.T) {
	fsys := fstest.MapFS{
		"provider.yaml": {Data: []byte("name: p\nproviderServices:\n  things:\n    service: {$ref: 'services/things.yaml'}\n")},
		"services/things.yaml": {Data: []byte(`openapi: 3.0.0
servers: [{url: 'https://example.test'}]
paths:
  /widgets:
    get:
      operationId: listWidgets
      responses:
        '200': {description: ok}
components:
  x-stackQL-resources:
    widgets:
      methods:
        list:
          operation: {$ref: get}
          path: {$ref: /widgets}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/widgets/methods/list'}]
`)},
	}
	catalog, err := stackqldoc.Open(fsys)
	if err != nil {
		t.Fatal(err)
	}
	exchange, err := catalog.Exchange("stackql_unstable_p.things.widgets")
	if err != nil {
		t.Fatal(err)
	}
	if exchange.OperationID() != "listWidgets" || exchange.Request().URL() != "https://example.test/widgets" {
		t.Fatalf("wrong operation: %s %s", exchange.OperationID(), exchange.Request().URL())
	}
}

func TestResponseSchemaUsesDeclaredMediaType(t *testing.T) {
	fsys := fstest.MapFS{
		"provider.yaml": {Data: []byte(`name: p
providerServices:
  activity:
    service: {$ref: 'services/activity.yaml'}
`)},
		"services/activity.yaml": {Data: []byte(`openapi: 3.0.0
servers: [{url: 'https://example.test'}]
paths:
  /stars:
    get:
      responses:
        '200':
          content:
            application/json:
              schema: {type: array, items: {type: object, properties: {login: {type: string}}}}
            application/vnd.github.star+json:
              schema: {type: array, items: {type: object, properties: {user: {type: object}}}}
components:
  x-stackQL-resources:
    stars:
      methods:
        list:
          operation: {$ref: '#/paths/~1stars/get'}
          response: {mediaType: application/json}
    extended:
      methods:
        list:
          operation: {$ref: '#/paths/~1stars/get'}
          response: {mediaType: application/vnd.github.star+json}
`)},
	}
	for range 50 {
		cat, err := stackqldoc.Open(fsys)
		if err != nil {
			t.Fatal(err)
		}
		for resource, field := range map[string]string{"stars": "login", "extended": "user"} {
			ex, err := cat.Exchange("stackql_unstable_p.activity." + resource)
			if err != nil {
				t.Fatal(err)
			}
			row, ok := ex.Response().Schema().Items()
			if !ok {
				t.Fatal("missing item schema")
			}
			if _, ok := row.Property(field); !ok {
				t.Fatalf("%s lost %s", resource, field)
			}
		}
	}
}
