package docx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
)

func TestLegacyXMLKeepsTransformedBooleansTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, "<Response/>")
	}))
	defer srv.Close()
	source := []byte(`openapi: 3.0.0
servers: [{url: 'http://example.test'}]
paths:
  /items:
    get:
      responses:
        '200':
          content:
            application/xml:
              schema: {type: object}
components:
  x-stackQL-resources:
    items:
      methods:
        list:
          operation: {$ref: '#/paths/~1items/get'}
          response:
            mediaType: application/xml
            overrideMediaType: application/json
            transform:
              type: golang_template_v0.1.0
              body: '[{"enabled":true,"disabled":false,"count":1}]'
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/items/methods/list'}]
`)
	p, err := docx.SelectPlan(source, "items", nil, registry(t), docx.WithBaseURL(srv.URL), docx.WithLegacyXMLValues())
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, p)
	if len(rows) != 1 {
		t.Fatalf("rows=%v", rows)
	}
	if rows[0]["enabled"] != true || rows[0]["disabled"] != false {
		t.Fatalf("boolean types lost: %#v", rows[0])
	}
}
