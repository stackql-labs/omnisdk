package omnisdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

func TestReadHintsConsumeOffsetOnlyBeforeUnfilteredOrderedRows(t *testing.T) {
	for _, kind := range []string{"offset", "local_filter", "unsupported_order"} {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			var asked map[string]string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = map[string]string{}
				for k, v := range r.URL.Query() {
					asked[k] = v[0]
				}
				mu.Unlock()
				items := []map[string]any{{"id": 1, "name": "a"}, {"id": 2, "name": "b"}, {"id": 3, "name": "c"}}
				if skip, _ := strconv.Atoi(r.URL.Query().Get("$skip")); skip > 0 {
					if skip > len(items) {
						skip = len(items)
					}
					items = items[skip:]
				}
				if top, _ := strconv.Atoi(r.URL.Query().Get("$top")); top > 0 && top < len(items) {
					items = items[:top]
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
			}))
			defer srv.Close()
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "services"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "provider.yaml"), []byte("name: p\nproviderServices:\n  s:\n    name: s\n    service: {$ref: 'services/s.yaml'}\n"), 0644); err != nil {
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
                        id: {type: integer}
                        name: {type: string}
components:
  x-stackQL-resources:
    items:
      config:
        queryParamPushdown:
          top: {dialect: odata}
          skip: {dialect: odata}
          orderBy: {dialect: odata, supportedColumns: [id]}
          filter: {dialect: odata, supportedOperators: [eq]}
      methods:
        list:
          operation: {$ref: '#/paths/~1items/get'}
          response: {mediaType: application/json, objectKey: '$.items'}
`
			if err := os.WriteFile(filepath.Join(dir, "services/s.yaml"), []byte(doc), 0644); err != nil {
				t.Fatal(err)
			}
			var where []query.Predicate
			hint := omnisdk.ReadHints{Limit: 1, Offset: 1}
			want := []string{"2"}
			offset := 1
			if kind == "local_filter" {
				where = []query.Predicate{query.NewCompare(query.Gt, query.NewColumn("i", "id"), query.NewLiteral(1))}
				want = []string{"2", "3"}
				offset = 0
			}
			if kind == "unsupported_order" {
				hint.OrderBy = []omnisdk.ReadOrder{{Column: "name", Descending: true}}
				want = []string{"1", "2", "3"}
				offset = 0
			}
			q, err := query.New([]query.Join{query.NewJoin(query.NewResource("i", "p.s.items"), query.Base)}, where, []query.Output{query.NewOutput("id", query.NewColumn("i", "id"))})
			if err != nil {
				t.Fatal(err)
			}
			table, err := omnisdk.DescribeTable(dir, "stackql_unstable_p.s.items")
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"i": table})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := omnisdk.NewGraphQuery(dir, resolved.Graph(), omnisdk.Args{Endpoint: srv.URL, Params: resolved.Params(), ReadHintsByAlias: map[string]omnisdk.ReadHints{"i": hint}})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := plan.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			effects, ok := rows.(omnisdk.ReadEffects)
			if !ok || effects.AppliedOffset() != offset {
				t.Fatalf("offset acknowledgment = %v", effects)
			}
			got := []string{}
			for rows.Next() {
				got = append(got, fmt.Sprint(rows.Row()["id"]))
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("rows = %v, want %v", got, want)
			}
			mu.Lock()
			defer mu.Unlock()
			if offset == 0 && asked["$skip"] != "" {
				t.Fatalf("unsafe offset pushed: %v", asked)
			}
			if offset == 1 && asked["$skip"] != "1" {
				t.Fatalf("offset not pushed: %v", asked)
			}
		})
	}
}
