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
