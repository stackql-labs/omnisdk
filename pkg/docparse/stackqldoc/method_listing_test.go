package stackqldoc_test

import (
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
	"reflect"
	"testing"
)

func TestMethodListingRetainsUnmappedGetAndViewInputs(t *testing.T) {
	doc, err := stackqldoc.Parse([]byte(`openapi: 3.0.0
paths:
  /items:
    get:
      responses:
        '200': {description: ok}
components:
  x-stackQL-resources:
    items:
      config:
        views:
          select:
            predicate: 'sqlDialect == "sqlite3" && requiredParams == [ "data__Identifier", region ]'
            ddl: 'select * from items'
      methods:
        get: {operation: {$ref: '#/paths/~1items/get'}}
        list: {operation: {$ref: '#/paths/~1items/get'}}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/items/methods/list'}]
`))
	if err != nil {
		t.Fatal(err)
	}
	methods, err := doc.Methods("items")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range methods {
		if method.SQLVerb() != "select" {
			t.Fatalf("%s verb=%s", method.Name(), method.SQLVerb())
		}
		if method.Name() == "__view__" && !reflect.DeepEqual(method.(aot.MethodInputs).RequiredParams(), []string{"data__Identifier", "region"}) {
			t.Fatal("view inputs lost")
		}
	}
}
