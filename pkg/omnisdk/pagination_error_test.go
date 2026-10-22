package omnisdk_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

func TestCompletedPageSurvivesContinuationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") != "" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid page"}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"id":"one"},{"id":"two"}],"next":"page2"}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider.yaml"), []byte("name: p\nproviderServices:\n  s:\n    service: {$ref: 'services/s.yaml'}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	doc := `openapi: 3.0.0
servers: [{url: 'https://example.test'}]
paths:
  /items:
    get:
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  items:
                    type: array
                    items:
                      type: object
                      properties:
                        id: {type: string}
components:
  x-stackQL-resources:
    items:
      config:
        pagination:
          requestToken: {key: page, location: query}
          responseToken: {key: '$.next', location: body}
      methods:
        list:
          operation: {$ref: '#/paths/~1items/get'}
          response: {mediaType: application/json, objectKey: '$.items'}
`
	if err := os.WriteFile(filepath.Join(dir, "services/s.yaml"), []byte(doc), 0644); err != nil {
		t.Fatal(err)
	}
	tab, err := omnisdk.DescribeTable(dir, "stackql_unstable_p.s.items")
	if err != nil {
		t.Fatal(err)
	}
	q, err := query.New([]query.Join{query.NewJoin(query.NewResource("i", "p.s.items"), query.Base)}, nil, []query.Output{query.NewOutput("id", query.NewColumn("i", "id"))})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"i": tab})
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
	ids := []string{}
	for rows.Next() {
		ids = append(ids, fmt.Sprint(rows.Row()["id"]))
	}
	if strings.Join(ids, ",") != "one,two" {
		t.Fatalf("completed rows = %v", ids)
	}
	if rows.Err() == nil || !strings.Contains(rows.Err().Error(), "invalid page") {
		t.Fatalf("continuation failure = %v", rows.Err())
	}
	var response omnisdk.HTTPStatusError
	if !errors.As(rows.Err(), &response) || response.StatusCode() != 400 || !strings.Contains(response.ResponseBody(), "invalid page") || !strings.Contains(response.RequestURL(), "page=page2") {
		t.Fatalf("HTTP response metadata = %v", rows.Err())
	}
}
