package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/docparse/dsl"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

const admissionService = `openapi: 3.0.0
servers: [{url: 'https://example.test'}]
paths:
  /databases:
    post:
      operationId: create_db
      responses:
        '200':
          content:
            application/json:
              schema: {type: object, properties: {id: {type: string}, operation: {type: string}}}
  /databases/{id}:
    get:
      operationId: get_db
      parameters: [{name: id, in: path, required: true, schema: {type: string}}]
      responses:
        '200':
          content:
            application/json:
              schema: {type: object, properties: {id: {type: string}, state: {type: string}}}
    patch:
      operationId: update_db
      parameters: [{name: id, in: path, required: true, schema: {type: string}}]
      requestBody:
        content:
          application/json:
            schema: {type: object, properties: {size: {type: string}}}
      responses:
        '200':
          content:
            application/json:
              schema: {type: object, properties: {id: {type: string}}}
  /operations/{id}:
    get:
      operationId: get_operation
      parameters: [{name: id, in: path, required: true, schema: {type: string}}]
      responses:
        '200':
          content:
            application/json:
              schema: {type: object, properties: {status: {type: string}}}
components:
  x-stackQL-resources:
    databases:
      methods:
        create: {operation: {$ref: '#/paths/~1databases/post'}, response: {mediaType: application/json, openAPIDocKey: '200'}}
        get: {operation: {$ref: '#/paths/~1databases~1{id}/get'}, response: {mediaType: application/json, openAPIDocKey: '200'}}
        update: {operation: {$ref: '#/paths/~1databases~1{id}/patch'}, response: {mediaType: application/json, openAPIDocKey: '200'}}
      sqlVerbs:
        insert: [{$ref: '#/components/x-stackQL-resources/databases/methods/create'}]
        select: [{$ref: '#/components/x-stackQL-resources/databases/methods/get'}]
        update: [{$ref: '#/components/x-stackQL-resources/databases/methods/update'}]
    operations:
      methods:
        get: {operation: {$ref: '#/paths/~1operations~1{id}/get'}, response: {mediaType: application/json, openAPIDocKey: '200'}}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/operations/methods/get'}]
`

func admissionRegistry(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "demo", "v00.00.00000")
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	provider := `name: demo
version: v00.00.00000
providerServices:
  db:
    id: demo:db
    name: db
    preferred: true
    service: {$ref: demo/v00.00.00000/services/db.yaml}
`
	if err := os.WriteFile(filepath.Join(dir, "provider.yaml"), []byte(provider), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "services", "db.yaml"), []byte(admissionService), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}
func admissionGraph(t *testing.T, body string) omnisdk.Graph {
	t.Helper()
	graph, err := omnisdk.NewGraph([]omnisdk.Node{
		omnisdk.NewMutationNode("create", "stackql_unstable_demo.db.databases", "insert", nil, nil, nil),
		omnisdk.NewNode("operation", "stackql_unstable_demo.db.operations", nil),
		omnisdk.NewNode("detail", "stackql_unstable_demo.db.databases", nil),
		omnisdk.NewMutationNode("update", "stackql_unstable_demo.db.databases", "update", nil, nil, map[string]any{"size": "L"}),
	}, []omnisdk.Wiring{
		omnisdk.NewWiring("operation", []omnisdk.Inbound{omnisdk.NewInbound("create", "operation", "id")}, "", ""),
		omnisdk.NewWiring("detail", []omnisdk.Inbound{omnisdk.NewInbound("create", "id", "id")}, "", ""),
		omnisdk.NewWiringWithTransform("update", []omnisdk.Inbound{
			omnisdk.NewInbound("create", "id", "id"), omnisdk.NewInbound("operation", "status", "operation"), omnisdk.NewInbound("detail", "state", "detail"),
		}, dsl.Transform{Type: "starlark", Body: body, Admission: &dsl.Program{Type: "starlark", Body: `(edges["operation"].present and edges["operation"].value == "DONE") or (edges["detail"].present and re.fullmatch("RUNNING|AVAILABLE", edges["detail"].value))`}}, "id"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func TestStarlarkAdmissionMockDatabase(t *testing.T) {
	registry := admissionRegistry(t)
	var creates, updates atomic.Int32
	slowStarted := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST":
			creates.Add(1)
			fmt.Fprint(w, `{"id":"db-1","operation":"op-1"}`)
		case r.Method == "PATCH":
			updates.Add(1)
			fmt.Fprint(w, `{"id":"db-1"}`)
		case strings.HasPrefix(r.URL.Path, "/operations/"):
			close(slowStarted)
			<-r.Context().Done()
		default:
			select {
			case <-slowStarted:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, `{"id":"db-1","state":"RUNNING"}`)
		}
	}))
	defer srv.Close()
	graph := admissionGraph(t, `{"id": inbox["id"]}`)
	pl, err := omnisdk.NewGraphQuery(registry, graph, omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := pl.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 || updates.Load() != 1 {
		t.Fatalf("creates=%d updates=%d", creates.Load(), updates.Load())
	}
}
func TestStarlarkRejectedBeforeCreate(t *testing.T) {
	if _, err := omnisdk.NewGraphQuery(admissionRegistry(t), admissionGraph(t, `"x" * 1000000000000`), omnisdk.Args{}); err == nil {
		t.Fatal("unbounded transform accepted during planning")
	}
}
