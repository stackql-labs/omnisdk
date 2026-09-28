package doclint_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stackql-labs/omnisdk/pkg/docparse/doclint"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

const provider = `id: p
name: p
version: v1
providerServices:
  s:
    name: s
    preferred: true
    service: {$ref: p/v1/services/s.yaml}
  gone:
    name: gone
    service: {$ref: p/v1/services/gone.yaml}
`

// Each resource trips one rule: envelope rows with no key, rows at the default (clean), two methods
// needing the same parameters, a select with no schema, and a parameter $ref to nowhere.
const service = `openapi: 3.0.0
info: {title: s, version: v1}
servers: [{url: "https://api.example.com"}]
paths:
  /envelope:
    get:
      responses:
        '200': {content: {application/json: {schema: {type: object, properties: {kind: {type: string}, things: {type: array, items: {type: object, properties: {id: {type: string}}}}}}}}}
  /defaulted:
    get:
      responses:
        '200': {content: {application/json: {schema: {type: object, properties: {items: {type: array, items: {type: object, properties: {id: {type: string}}}}}}}}}
  /a/{x}:
    get:
      parameters: [{name: x, in: path, required: true}]
      responses: {'200': {content: {application/json: {schema: {type: object, properties: {id: {type: string}}}}}}}
  /b/{x}:
    get:
      parameters: [{name: x, in: path, required: true}]
      responses: {'200': {content: {application/json: {schema: {type: object, properties: {id: {type: string}}}}}}}
  /bare:
    get:
      responses: {'200': {description: ok}}
  /broken:
    get:
      parameters: [{$ref: '#/components/parameters/nowhere'}]
      responses: {'200': {content: {application/json: {schema: {type: object}}}}}
components:
  x-stackQL-resources:
    envelope: {id: p.s.envelope, name: envelope, methods: {list: {operation: {$ref: '#/paths/~1envelope/get'}, response: {mediaType: application/json}}}, sqlVerbs: {select: [{$ref: '#/components/x-stackQL-resources/envelope/methods/list'}]}}
    defaulted: {id: p.s.defaulted, name: defaulted, methods: {list: {operation: {$ref: '#/paths/~1defaulted/get'}, response: {mediaType: application/json}}}, sqlVerbs: {select: [{$ref: '#/components/x-stackQL-resources/defaulted/methods/list'}]}}
    twins:
      id: p.s.twins
      name: twins
      methods:
        a: {operation: {$ref: '#/paths/~1a~1{x}/get'}, response: {mediaType: application/json}}
        b: {operation: {$ref: '#/paths/~1b~1{x}/get'}, response: {mediaType: application/json}}
      sqlVerbs: {select: [{$ref: '#/components/x-stackQL-resources/twins/methods/a'}, {$ref: '#/components/x-stackQL-resources/twins/methods/b'}]}
    bare: {id: p.s.bare, name: bare, methods: {get: {operation: {$ref: '#/paths/~1bare/get'}, response: {mediaType: application/json}}}, sqlVerbs: {select: [{$ref: '#/components/x-stackQL-resources/bare/methods/get'}]}}
    broken: {id: p.s.broken, name: broken, methods: {get: {operation: {$ref: '#/paths/~1broken/get'}, response: {mediaType: application/json}}}, sqlVerbs: {select: [{$ref: '#/components/x-stackQL-resources/broken/methods/get'}]}}
`

func TestStackQLFamily(t *testing.T) {
	reg, err := stackqldoc.OpenRegistry(fstest.MapFS{
		"p/v1/provider.yaml":   {Data: []byte(provider)},
		"p/v1/services/s.yaml": {Data: []byte(service)},
	})
	if err != nil {
		t.Fatal(err)
	}
	family, ok := doclint.For(doclint.Format)
	if !ok {
		t.Fatal("no stackql family")
	}
	got := map[string]string{}
	for _, f := range doclint.Run(reg, family) {
		got[f.Rule+" "+f.Address] = f.Message
	}
	for _, want := range []string{
		"missing_document stackql_unstable_p.gone",
		"rows_not_located stackql_unstable_p.s.envelope",
		"indistinguishable_selects stackql_unstable_p.s.twins",
		"no_row_schema stackql_unstable_p.s.bare",
		"resolves stackql_unstable_p.s.broken",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing finding %q; got %v", want, got)
		}
	}
	for k := range got {
		if strings.HasSuffix(k, ".defaulted") {
			t.Errorf("rows at the default $.items reported: %s", k)
		}
	}
	if !strings.Contains(got["rows_not_located stackql_unstable_p.s.envelope"], "$.things") {
		t.Errorf("envelope finding does not name the list: %q", got["rows_not_located stackql_unstable_p.s.envelope"])
	}
}
