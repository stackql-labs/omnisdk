package stackqldoc_test

import (
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
	"strings"
	"testing"
)

func TestRequestConfigurationUsesNearestDeclaration(t *testing.T) {
	doc, err := stackqldoc.Parse([]byte(`openapi: 3.0.0
servers: [{url: 'https://example.test', variables: {region: {}}}]
x-stackQL-config:
  requestTranslate: {algorithm: document}
  queryParamTranspose: {algorithm: document}
paths:
  /items:
    get:
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [TypeName]
              properties:
                TypeName: {type: string}
      responses:
        '200':
          content:
            application/json:
              schema: {type: object}
components:
  x-stackQL-resources:
    document:
      methods:
        select: {operation: {$ref: '#/paths/~1items/get'}}
    resource:
      config:
        requestTranslate: {algorithm: resource}
        queryParamTranspose: {algorithm: resource}
      methods:
        select: {operation: {$ref: '#/paths/~1items/get'}}
    method:
      config:
        requestTranslate: {algorithm: resource}
        queryParamTranspose: {algorithm: resource}
      methods:
        select:
          config:
            requestTranslate: {algorithm: method}
            queryParamTranspose: {algorithm: method}
          request: {base: '{"TypeName":"fallback"}'}
          operation: {$ref: '#/paths/~1items/get'}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"document", "resource", "method"} {
		ex, err := doc.Select(name)
		if err != nil {
			t.Fatal(err)
		}
		req := ex.Request()
		if req.Translate() != name || req.Transpose() != name {
			t.Fatalf("%s configuration = %q,%q", name, req.Translate(), req.Transpose())
		}
		found := false
		for _, p := range req.Parameters() {
			if p.Name() == "TypeName" && p.In() == "body" {
				found = true
			}
		}
		if !found {
			t.Fatal("declared request body field is absent from input vocabulary")
		}
		methods, err := doc.Methods(name)
		if err != nil {
			t.Fatal(err)
		}
		required := methods[0].(aot.MethodInputs).RequiredParams()
		want := "data__TypeName,region"
		if name == "method" {
			want = "region"
		}
		if strings.Join(required, ",") != want {
			t.Fatalf("%s required parameters = %v, want %s", name, required, want)
		}
	}
}

func TestServerEnvironmentAnnotationsAreMetadataOnly(t *testing.T) {
	t.Setenv("OMNI_METADATA_ONLY_REGION", "must-not-be-read")
	doc, err := stackqldoc.Parse([]byte(`openapi: 3.0.0
servers:
  - url: 'https://{region}.example.test'
    variables:
      region: {default: ignored, x-stackQL-envVar: OMNI_METADATA_ONLY_REGION}
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
	request := exchange.Request()
	if request.URL() != "https://{region}.example.test/items" {
		t.Fatalf("parser resolved scope: %s", request.URL())
	}
	metadata := request.(aot.ServerEnvironmentBindings).ServerEnvironmentBindings()
	if metadata["region"] != "OMNI_METADATA_ONLY_REGION" {
		t.Fatalf("metadata=%v", metadata)
	}
	metadata["region"] = "changed"
	if request.(aot.ServerEnvironmentBindings).ServerEnvironmentBindings()["region"] != "OMNI_METADATA_ONLY_REGION" {
		t.Fatal("metadata exposes mutable parser state")
	}
	methods, err := doc.Methods("items")
	if err != nil {
		t.Fatal(err)
	}
	if got := methods[0].(aot.ServerEnvironmentBindings).ServerEnvironmentBindings()["region"]; got != "OMNI_METADATA_ONLY_REGION" {
		t.Fatalf("method metadata=%s", got)
	}
}

func TestServerTransportDefaultsNeverIncludeScope(t *testing.T) {
	doc, err := stackqldoc.Parse([]byte(`openapi: 3.0.0
servers:
  - url: '{scheme}://{host}/{region}'
    variables:
      scheme: {default: https}
      host: {default: ignored.example.test}
      region: {default: ignored-region}
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
	metadata := exchange.Request().(aot.ServerTransportDefaults).ServerTransportDefaults()
	if len(metadata) != 1 || metadata["scheme"] != "https" {
		t.Fatalf("transport metadata=%v", metadata)
	}
	if exchange.Request().URL() != "{scheme}://{host}/{region}/items" {
		t.Fatal("parser substituted default scope or transport")
	}
	metadata["scheme"] = "changed"
	if exchange.Request().(aot.ServerTransportDefaults).ServerTransportDefaults()["scheme"] != "https" {
		t.Fatal("transport metadata exposes mutable parser state")
	}
	methods, err := doc.Methods("items")
	if err != nil {
		t.Fatal(err)
	}
	if methods[0].(aot.ServerTransportDefaults).ServerTransportDefaults()["scheme"] != "https" {
		t.Fatal("discovery lost transport metadata")
	}
}
