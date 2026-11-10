package docx_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
)

const transformedGraphQLDocument = `openapi: 3.0.0
info: {title: GraphQL response transform regression, version: '1'}
servers: [{url: 'https://example.invalid'}]
paths:
  /graphql:
    post:
      operationId: analytics
      x-stackql-protocol: graphql
      x-stackQL-graphQL:
        url: https://example.invalid/graphql
        httpVerb: POST
        query: 'query { viewer { zones(first: {{ .limit }}{{ .cursor }}) { httpRequests1hGroups { dimensions { datetime } sum { requests } } } } }'
        responseSelection: {jsonPath: '$.result[*]'}
        cursor:
          strategy: page_info
          jsonPath: $.next
          terminateOnJsonPath: $.more
      responses:
        '200':
          description: Analytics
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Envelope'}
components:
  schemas:
    Envelope:
      type: object
      properties:
        result:
          type: array
          items:
            type: object
            properties:
              datetime: {type: string}
              requests: {type: integer}
        next: {type: string}
        more: {type: boolean}
  x-stackQL-resources:
    analytics:
      id: demo.analytics
      name: analytics
      methods:
        list:
          operation: {$ref: '#/paths/~1graphql/post'}
          response:
            mediaType: application/json
            openAPIDocKey: '200'
            objectKey: $.result
            transform:
              type: golang_template_json_v0.3.0
              body: '__PROGRAM__'
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/analytics/methods/list'}]
`
const graphQLFlattenProgram = `{{ $zone := index . "data" "viewer" "zones" 0 }}{"result":[{{ $sep := separator "," }}{{ range index $zone "httpRequests1hGroups" }}{{ call $sep }}{"datetime":{{ toJson (index . "dimensions" "datetime") }},"requests":{{ toJson (index . "sum" "requests") }}}{{ end }}],"next":{{ toJson (index $zone "cursor") }},"more":{{ toJson (index $zone "more") }}}`

func graphQLResult(t *testing.T, server *httptest.Server, body string, strict bool, extra ...docx.Option) ([]map[string]any, error) {
	t.Helper()
	document := strings.Replace(transformedGraphQLDocument, "__PROGRAM__", body, 1)
	options := append([]docx.Option{docx.WithBaseURL(server.URL)}, extra...)
	if strict {
		options = append(options, docx.WithStrictSelection())
	}
	p, err := docx.SelectPlan([]byte(document), "analytics", nil, registry(t), options...)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	records := plan.ComposeRows(1, p).Open(ctx)
	defer records.Close()
	var rows []map[string]any
	for records.Next(ctx) {
		row, _ := bind.DocMap(records.Record())
		rows = append(rows, row)
	}
	return rows, records.Err()
}

func TestGraphQLResponseProgramPrecedesRowsAndPagination(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := calls.Add(1)
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if page == 2 && !strings.Contains(body["query"], `after: "next-1"`) {
			t.Errorf("transformed cursor missing: %s", body["query"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"viewer":{"zones":[{"httpRequests1hGroups":[{"dimensions":{"datetime":"hour-%d"},"sum":{"requests":%d}}],"cursor":"next-%d","more":%t}]}}}`, page, page*10, page, page < 2)
	}))
	defer server.Close()
	rows, err := graphQLResult(t, server, graphQLFlattenProgram, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || calls.Load() != 2 {
		t.Fatalf("rows=%v requests=%d; want two transformed pages", rows, calls.Load())
	}
	if rows[0]["datetime"] != "hour-1" || rows[1]["datetime"] != "hour-2" || fmt.Sprint(rows[1]["requests"]) != "20" {
		t.Fatalf("wrong transformed rows: %v", rows)
	}
}

func TestGraphQLResponseProgramFailuresAndEmptySelection(t *testing.T) {
	for _, test := range []struct {
		name, reply, program string
		strict               bool
		missing              bool
		wantError            string
	}{
		{"missing transformed key", `{"data":{}}`, `{}`, true, true, "unknown key result"},
		{"transformed document replaces raw keys", `{"result":[{"datetime":"raw"}],"data":{}}`, `{}`, true, true, "unknown key result"},
		{"empty transformed list", `{"data":{}}`, `{"result":[]}`, true, false, ""},
		{"permissive missing key", `{"data":{}}`, `{}`, false, false, ""},
		{"invalid program", `{"data":{}}`, `{{`, true, false, "parse:"},
		{"invalid transformed JSON", `{"data":{}}`, `not-json`, true, false, "graphql response"},
		{"GraphQL errors cannot be hidden", `{"errors":[{"message":"denied"}]}`, `{"result":[{"datetime":"fabricated"}],"more":false}`, true, false, "graphql response errors"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, test.reply)
			}))
			defer server.Close()
			rows, err := graphQLResult(t, server, test.program, test.strict)
			if len(rows) != 0 {
				t.Fatalf("unexpected rows: %v", rows)
			}
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error=%v want %q", err, test.wantError)
			}
			if test.missing {
				var selection facade.ResponseSelectionError
				if !errors.As(err, &selection) || selection.MissingKey() != "result" {
					t.Fatalf("missing-selection error type lost: %v", err)
				}
			}
		})
	}
}

func TestGraphQLResponseProgramMayProduceArray(t *testing.T) {
	document := strings.Replace(transformedGraphQLDocument, "__PROGRAM__", `[{"datetime":"flattened","requests":7}]`, 1)
	document = strings.Replace(document, "responseSelection: {jsonPath: '$.result[*]'}", "responseSelection: {jsonPath: '$[*]'}", 1)
	document = strings.Replace(document, "objectKey: $.result", "objectKey: $", 1)
	// This single-page declaration has no cursor or termination metadata.
	begin := strings.Index(document, "        cursor:\n")
	end := strings.Index(document[begin:], "      responses:") + begin
	document = document[:begin] + document[end:]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{}}`)
	}))
	defer server.Close()
	p, err := docx.SelectPlan([]byte(document), "analytics", nil, registry(t), docx.WithBaseURL(server.URL), docx.WithStrictSelection())
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, p)
	if len(rows) != 1 || rows[0]["datetime"] != "flattened" {
		t.Fatalf("array transform lost rows: %v", rows)
	}
}

type graphQLRawObserver struct {
	t         *testing.T
	responses atomic.Int32
}

func (*graphQLRawObserver) HTTPRequest(string, string, []byte) {}
func (*graphQLRawObserver) HTTPRows(any)                       {}
func (o *graphQLRawObserver) HTTPResponse(status int, body []byte) {
	o.responses.Add(1)
	if status != 200 || string(body) != `{"data":{"wire":"original"}}` {
		o.t.Errorf("observer saw transformed response: status=%d body=%s", status, body)
	}
}

func TestGraphQLResponseObserverRetainsRawBody(t *testing.T) {
	observer := &graphQLRawObserver{t: t}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"wire":"original"}}`)
	}))
	defer server.Close()
	rows, err := graphQLResult(t, server, `{"result":[{"datetime":"transformed","requests":1}],"more":false}`, true, docx.WithHTTPObserver(observer))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["datetime"] != "transformed" || observer.responses.Load() != 1 {
		t.Fatalf("rows=%v observed responses=%d", rows, observer.responses.Load())
	}
}
