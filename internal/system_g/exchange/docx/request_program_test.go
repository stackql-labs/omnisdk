package docx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

const transformedRequestXML = `<Envelope><Status>Enabled</Status></Envelope>`

type requestProgramSigner struct{ t *testing.T }

func (s requestProgramSigner) Apply(in facade.Page) (facade.Record, error) {
	if got := string(httpx.ReqBody(in)); got != transformedRequestXML {
		s.t.Errorf("signed body=%s", got)
	}
	header := httpx.Header(in)
	header.Set("X-Body-Checked", "true")
	return httpx.NewRequestRecord(httpx.Method(in), httpx.URL(in), header, httpx.ReqBody(in)), nil
}

func TestDeclaredRequestProgramProducesWireBodyBeforeSigning(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if string(body) != transformedRequestXML || r.Header.Get("Content-Type") != "application/xml" || r.Header.Get("X-Body-Checked") != "true" {
			t.Errorf("body=%s headers=%v", body, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	source := []byte(`openapi: 3.0.0
servers: [{url: 'http://example.test'}]
paths:
  /items:
    post:
      requestBody:
        content:
          application/xml:
            schema:
              title: Envelope
              type: object
              properties:
                Status: {type: string}
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
          request:
            mediaType: application/xml
            transform:
              type: golang_template_text_v0.1.0
              body: '<Envelope><Status>{{ index (jsonMapFromString .) "Status" }}</Status></Envelope>'
          response: {mediaType: application/json}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/items/methods/create'}]
`)
	plan, err := docx.SelectPlan(source, "items", map[string]any{"Status": "Enabled"}, registry(t), docx.WithBaseURL(srv.URL), docx.WithRequestTransform(requestProgramSigner{t}))
	if err != nil {
		t.Fatal(err)
	}
	if rows := drain(t, plan); len(rows) != 1 {
		t.Fatalf("rows=%v", rows)
	}
	unsupported := []byte(strings.ReplaceAll(string(source), "golang_template_text_v0.1.0", "golang_template_text_v0.1.999"))
	_, err = docx.SelectPlan(unsupported, "items", map[string]any{"Status": "Enabled"}, registry(t), docx.WithBaseURL(srv.URL))
	if err == nil || err.Error() != "unsupported template type: golang_template_text_v0.1.999" {
		t.Fatalf("error=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("unsupported program sent a request: %d calls", calls.Load())
	}
}
