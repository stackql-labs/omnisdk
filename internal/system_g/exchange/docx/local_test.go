package docx

import (
	"context"
	"encoding/json"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl/gotemplate"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
	"testing"
)

func TestInlineCommandBindsArgvAndTransformsResponse(t *testing.T) {
	doc, err := stackqldoc.Parse([]byte(`resources:
  cert:
    methods:
      get:
        inline: [printf, 'name=%s', '{{ .parameters.name }}']
        parameters:
          name: {in: inline, required: true}
        response:
          schema_override: {type: object, properties: {name: {type: string}}}
          transform:
            type: golang_template_v0.1.0
            body: '[{"name":"{{getRegexpFirstMatch . "name=(.*)"}}"}]'
    sqlVerbs:
      select: [{$ref: '#/components/x-stackQL-resources/cert/methods/get'}]
`))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := doc.Select("cert")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := dsl.NewRegistry(gotemplate.Evaluators()...)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := Spec(ex, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	rows := spec.Make(map[string]any{"name": "literal; echo should-not-run"}).Open(context.Background())
	defer rows.Close()
	if !rows.Next(context.Background()) {
		t.Fatalf("missing result: %v", rows.Err())
	}
	value, ok := rows.Record().Doc(facade.AnonymousPayload)
	if !ok {
		t.Fatal("missing row document")
	}
	encoded, _ := json.Marshal(value)
	if string(encoded) != `{"name":"literal; echo should-not-run"}` {
		t.Fatalf("row=%s", encoded)
	}
	if rows.Next(context.Background()) || rows.Err() != nil {
		t.Fatalf("unexpected tail: %v", rows.Err())
	}
}
