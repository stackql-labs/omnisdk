package stackqldoc_test

import (
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
	"testing"
)

func TestSelectorMetadataDoesNotBindScope(t *testing.T) {
	t.Setenv("SELECTOR_CLOUD", "azure")
	doc, err := stackqldoc.Parse([]byte(`openapi: 3.0.0
servers:
  - url: https://{host}
    variables:
      host:
        default: documentation-only.example
        x-stackQL-envVar: DIRECT_HOST
        x-stackQL-envVarSelector:
          envVar: SELECTOR_CLOUD
          default: aws
          values: {aws: aws.example, azure: azure.example}
paths:
  /items:
    get:
      responses:
        '200': {description: ok}
components:
  x-stackQL-resources:
    items:
      methods:
        select: {operation: {$ref: '#/paths/~1items/get'}}
`))
	if err != nil {
		t.Fatal(err)
	}
	exchange, err := doc.Select("items")
	if err != nil {
		t.Fatal(err)
	}
	if exchange.Request().URL() != "https://{host}/items" {
		t.Fatal("parser applied a scope default")
	}
	request := exchange.Request().(aot.ServerVariableSpecs)
	specs := request.ServerVariableSpecs()
	if specs["host"].Default != "documentation-only.example" || specs["host"].Environment != "DIRECT_HOST" {
		t.Fatalf("metadata=%v", specs)
	}
	specs["host"].Selector.Values["aws"] = "corrupted"
	delete(specs, "host")
	if request.ServerVariableSpecs()["host"].Selector.Values["aws"] != "aws.example" {
		t.Fatal("request metadata is mutable")
	}
	methods, err := doc.Methods("items")
	if err != nil {
		t.Fatal(err)
	}
	method := methods[0].(aot.ServerVariableSpecs)
	method.ServerVariableSpecs()["host"].Selector.Values["aws"] = "corrupted"
	if method.ServerVariableSpecs()["host"].Selector.Values["aws"] != "aws.example" {
		t.Fatal("method metadata is mutable")
	}
}
