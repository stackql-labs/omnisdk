package docx

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

func requestShape(t *testing.T) aot.RequestShape {
	t.Helper()
	d, err := stackqldoc.Parse([]byte(`openapi: 3.0.0
servers: [{url: 'http://example.test'}]
paths:
  /items:
    post:
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                ItemCount: {type: integer}
                Enabled: {type: boolean}
                Label: {type: string}
                Protocols: {type: array, items: {type: string}}
                Options: {type: object, properties: {Retries: {type: integer}}}
      responses:
        '200':
          content:
            application/json:
              schema: {type: object}
components:
  x-stackQL-resources:
    items:
      methods:
        create:
          operation: {$ref: '#/paths/~1items/post'}
          request: {mediaType: application/json, nativeCasing: pascal}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/items/methods/create'}]
`))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := d.Select("items")
	if err != nil {
		t.Fatal(err)
	}
	return ex.Request().(aot.RequestShape)
}

func TestBodyShapeParsesStructuredSQLStrings(t *testing.T) {
	transform := bodyShape{requestShape(t)}
	out, err := transform.Apply(httpx.NewRequestRecord("POST", "http://example.test/items", nil, []byte(`{"Protocols":"[\"SFTP\"]","Options":"{\"Retries\":\"2\"}"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(httpx.ReqBody(out), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["Protocols"], []any{"SFTP"}) || !reflect.DeepEqual(got["Options"], map[string]any{"Retries": float64(2)}) {
		t.Fatalf("body = %v", got)
	}
	for _, body := range []string{`{"Protocols":"{}"}`, `{"Protocols":"[1] trailing"}`, `{"Options":"[]"}`} {
		if _, err := transform.Apply(httpx.NewRequestRecord("POST", "http://example.test/items", nil, []byte(body))); err == nil {
			t.Errorf("accepted malformed container: %s", body)
		}
	}
}

func TestBodyShapeCoercesBeforeSending(t *testing.T) {
	transform := bodyShape{requestShape(t)}
	in := httpx.NewRequestRecord("POST", "http://example.test/items", nil, []byte(`{"item_count":"12345678901234567890","enabled":"true","label":42,"extra":{"untouched":true}}`))
	out, err := transform.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(httpx.ReqBody(out))))
	decoder.UseNumber()
	if err := decoder.Decode(&got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"ItemCount": json.Number("12345678901234567890"), "Enabled": true, "Label": "42", "extra": map[string]any{"untouched": true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %#v, want %#v", got, want)
	}
	for _, body := range []string{`{"item_count":"1.5"}`, `{"enabled":"maybe"}`, `{"ItemCount":1,"item_count":2}`} {
		if _, err := transform.Apply(httpx.NewRequestRecord("POST", "http://example.test/items", nil, []byte(body))); err == nil {
			t.Errorf("accepted invalid or duplicate body: %s", body)
		}
	}
}
