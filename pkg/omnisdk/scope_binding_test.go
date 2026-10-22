package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

func TestQualifiedScopeBindingDoesNotFilterResponseURI(t *testing.T) {
	for _, kind := range []string{"server", "path", "rewritten", "missing"} {
		t.Run(kind, func(t *testing.T) {
			pathParameter := kind == "path"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := "/items"
				if pathParameter {
					want = "/regions/r1/items"
				}
				if r.URL.Path != want {
					t.Errorf("path = %s, want %s", r.URL.Path, want)
				}
				w.Header().Set("Content-Type", "application/json")
				if kind == "missing" {
					fmt.Fprint(w, `{"items":[{"id":"one"}]}`)
				} else {
					fmt.Fprint(w, `{"items":[{"id":"one","region":"https://example.test/regions/r1"}]}`)
				}
			}))
			defer srv.Close()
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "services"), 0755); err != nil {
				t.Fatal(err)
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			write("provider.yaml", "name: p\nproviderServices:\n  s:\n    name: s\n    service: {$ref: 'services/s.yaml'}\n")
			path, params := "/items", ""
			if pathParameter {
				path = "/regions/{region}/items"
				params = "      parameters: [{name: region, in: path, required: true, schema: {type: string}}]\n"
			}
			serverURL := "https://{region}.example.test"
			if kind == "rewritten" {
				serverURL = srv.URL
			}
			write("services/s.yaml", fmt.Sprintf(`openapi: 3.0.0
servers: [{url: '%s', variables: {region: {}}}]
paths:
  %s:
    get:
%s      responses:
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
                        region: {type: string}
components:
  x-stackQL-resources:
    items:
      methods:
        list:
          operation: {$ref: '#/paths/%s/get'}
          response: {mediaType: application/json, objectKey: '$.items'}
`, serverURL, path, params, func() string {
				if pathParameter {
					return "~1regions~1{region}~1items"
				}
				return "~1items"
			}()))
			var scope query.Expr = query.NewLiteral("r1")
			if kind == "missing" {
				scope = query.NewCall("lower", query.NewLiteral("R1"))
			}
			q, err := query.New([]query.Join{query.NewJoin(query.NewResource("i", "p.s.items"), query.Base)}, []query.Predicate{query.NewEq(query.NewColumn("i", "region"), scope)}, []query.Output{query.NewOutput("id", query.NewColumn("i", "id")), query.NewOutput("region", query.NewColumn("i", "region"))})
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
			plan, err := omnisdk.NewGraphQuery(dir, resolved.Graph(), omnisdk.Args{Endpoint: srv.URL, Params: resolved.Params()})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := plan.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("scoped request lost its row: %v", rows.Err())
			}
			if rows.Row()["id"] != "one" {
				t.Fatalf("row = %v", rows.Row())
			}
			wantRegion := "https://example.test/regions/r1"
			if kind == "missing" {
				wantRegion = "r1"
			}
			if rows.Row()["region"] != wantRegion {
				t.Fatalf("region = %v", rows.Row()["region"])
			}
			if rows.Next() || rows.Err() != nil {
				t.Fatalf("unexpected extra result: %v", rows.Err())
			}
		})
	}
}
