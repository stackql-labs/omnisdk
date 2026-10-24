package omnisdk_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

func TestStarUsesSelectedOperationSchema(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"provider.yaml": "name: p\nproviderServices:\n  s:\n    name: s\n    service: {$ref: 'services/s.yaml'}\n",
		"services/s.yaml": `openapi: 3.0.0
servers: [{url: 'https://{region}.example.test', variables: {region: {}}}]
paths:
  /items:
    get:
      responses:
        '200':
          content:
            application/json:
              schema: {type: object, properties: {listOnly: {type: string}}}
  /items/{id}:
    get:
      parameters: [{name: id, in: path, required: true, schema: {type: string}}]
      responses:
        '200':
          content:
            application/json:
              schema: {type: object, properties: {detailOnly: {type: string}}}
components:
  x-stackQL-resources:
    items:
      methods:
        list: {operation: {$ref: '#/paths/~1items/get'}}
        get: {operation: {$ref: '#/paths/~1items~1{id}/get'}}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	table, err := omnisdk.DescribeTable(dir, "stackql_unstable_p.s.items")
	if err != nil {
		t.Fatal(err)
	}
	for _, detail := range []bool{false, true} {
		predicates := []query.Predicate{query.NewEq(query.NewColumn("i", "region"), query.NewLiteral("east"))}
		want := []string{"listOnly"}
		if detail {
			predicates = append(predicates, query.NewEq(query.NewColumn("i", "id"), query.NewLiteral("one")))
			want = []string{"detailOnly"}
		}
		q, err := query.New([]query.Join{query.NewJoin(query.NewResource("i", table.Address()), query.Base)}, predicates, []query.Output{query.NewOutput("", query.NewStar("i"))})
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"i": table})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, col := range resolved.Graph().Projections()[0].Columns() {
			if !strings.HasPrefix(col.Out(), "\x00") {
				names = append(names, col.Out())
			}
		}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("detail=%v: columns=%v want=%v", detail, names, want)
		}
	}
}
