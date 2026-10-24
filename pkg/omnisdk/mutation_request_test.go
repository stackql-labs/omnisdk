package omnisdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

func TestMutationSeparatesBodyFieldsFromURLScope(t *testing.T) {
	root := t.TempDir()
	provider := []byte("name: demo\nproviderServices:\n  service:\n    service: {$ref: service.yaml}\n")
	document := []byte(`openapi: 3.0.0
servers: [{url: https://example.test}]
paths:
  /zones/{zone}/items:
    post:
      parameters:
        - {name: zone, in: path, required: true, schema: {type: string}}
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                zone: {type: string}
                name: {type: string}
      responses:
        '200':
          content:
            application/json:
              schema: {type: object, properties: {name: {type: string}}}
components:
  x-stackQL-resources:
    items:
      methods:
        insert:
          operation: {$ref: '#/paths/~1zones~1{zone}~1items/post'}
          request: {required: [name]}
          response: {mediaType: application/json}
      sqlVerbs:
        insert: [{$ref: '#/components/x-stackQL-resources/items/methods/insert'}]
`)
	for name, data := range map[string][]byte{"provider.yaml": provider, "service.yaml": document} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	address := "stackql_unstable_demo.service.items"
	table, err := omnisdk.DescribeMutation(root, address, "insert")
	if err != nil {
		t.Fatal(err)
	}
	method := table.Methods()[0]
	required := method.(omnisdk.RequestMetadata).RequestBodySchema().(aot.RequiredSchema).RequiredProperties()
	if !slices.Contains(required, "name") {
		t.Fatalf("missing method-required field: %v", required)
	}
	for _, parameter := range method.Params() {
		if parameter.Name() == "name" && !parameter.Required() {
			t.Fatal("name is not required")
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zones/z/items" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["name"] != "new" || len(body) != 1 {
			t.Errorf("scope leaked into body: %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"new"}`)
	}))
	defer server.Close()
	unresolved, err := query.NewMutation(query.NewInsert(query.NewResource("i", address),
		query.NewAssignment("zone", query.NewLiteral("z")), query.NewAssignment("data__name", query.NewLiteral("new"))), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := omnisdk.Resolve(unresolved, map[string]omnisdk.Table{"i": table})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := omnisdk.NewGraphQuery(root, resolved.Graph(), omnisdk.Args{Endpoint: server.URL, Params: resolved.Params()})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := planned.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
