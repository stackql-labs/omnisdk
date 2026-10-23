package omnisdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

func TestExplicitReadBodyBindingsDoNotLeakIntoSameNamedQueryParameter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("body binding leaked into query: %s", r.URL.RawQuery)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["limit"] != float64(2) || body["startTime"] != float64(123) {
			t.Errorf("body=%#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"events":[{"timestamp":7}]}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"provider.yaml": "name: p\nproviderServices:\n  s:\n    service: {$ref: 'services/s.yaml'}\n",
		"services/s.yaml": `openapi: 3.0.0
servers: [{url: 'https://example.test'}]
paths:
  /events:
    post:
      parameters: [{name: limit, in: query, schema: {type: string}}]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                limit: {type: integer}
                startTime: {type: integer}
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  events:
                    type: array
                    items: {type: object, properties: {timestamp: {type: integer}}}
components:
  x-stackQL-resources:
    events:
      methods:
        list:
          operation: {$ref: '#/paths/~1events/post'}
          response: {mediaType: application/json, objectKey: events}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/events/methods/list'}]
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	table, err := omnisdk.DescribeTable(dir, "stackql_unstable_p.s.events")
	if err != nil {
		t.Fatal(err)
	}
	unresolved, err := query.New([]query.Join{query.NewJoin(query.NewResource("e", "p.s.events"), query.Base)}, []query.Predicate{
		query.NewEq(query.NewColumn("e", "data__limit"), query.NewLiteral(int64(2))),
		query.NewEq(query.NewColumn("e", "data__startTime"), query.NewLiteral(int64(123))),
	}, []query.Output{query.NewOutput("timestamp", query.NewColumn("e", "timestamp")), query.NewOutput("input_limit", query.NewColumn("e", "data__limit")), query.NewOutput("input_time", query.NewColumn("e", "data__startTime"))})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := omnisdk.Resolve(unresolved, map[string]omnisdk.Table{"e": table})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := omnisdk.NewGraphQuery(dir, resolved.Graph(), omnisdk.Args{Endpoint: server.URL, Params: resolved.Params()})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := plan.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no result: %v", rows.Err())
	}
	if got := rows.Row(); fmt.Sprint(got["input_limit"]) != "2" || fmt.Sprint(got["input_time"]) != "123" {
		t.Fatalf("body alias projection lost its value: %#v", got)
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("unexpected continuation: %v", rows.Err())
	}
}
