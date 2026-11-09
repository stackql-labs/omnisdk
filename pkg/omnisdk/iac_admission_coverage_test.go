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

// Exercises the observation/admission path under a real ledger and compensation,
// rather than treating a successful non-durable query graph as IaC coverage.
func TestIaCAdmissionCoverage(t *testing.T) {
	for _, mode := range []string{"operation", "detail", "expired-operation", "rejected-update", "no-evidence", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			registry := admissionRegistry(t)
			service := filepath.Join(registry, "demo/v00.00.00000/services/db.yaml")
			document := strings.Replace(admissionService, "    patch:", "    delete:\n      operationId: delete_db\n      parameters: [{name: id, in: path, required: true, schema: {type: string}}]\n      responses: {'204': {description: removed}}\n    patch:", 1)
			document = strings.Replace(document, "        update: {operation:", "        delete: {operation: {$ref: '#/paths/~1databases~1{id}/delete'}, response: {openAPIDocKey: '204'}}\n        update: {operation:", 1)
			document = strings.Replace(document, "        update: [{$ref:", "        delete: [{$ref: '#/components/x-stackQL-resources/databases/methods/delete'}]\n        update: [{$ref:", 1)
			if err := os.WriteFile(service, []byte(document), 0600); err != nil {
				t.Fatal(err)
			}
			var creates, updates, deletes atomic.Int32
			var ready atomic.Bool
			opStarted, detailStarted := make(chan struct{}), make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == "POST":
					creates.Add(1)
					fmt.Fprint(w, `{"id":"db-1","operation":"op-1"}`)
				case r.Method == "PATCH":
					updates.Add(1)
					if !ready.Load() {
						t.Error("update started without sufficient evidence")
					}
					if mode == "rejected-update" {
						http.Error(w, `{"error":"update refused"}`, 400)
						return
					}
					fmt.Fprint(w, `{"id":"db-1"}`)
				case r.Method == "DELETE":
					deletes.Add(1)
					w.WriteHeader(204)
				case strings.HasPrefix(r.URL.Path, "/operations/"):
					close(opStarted)
					if mode == "expired-operation" {
						http.Error(w, `{"error":"operation expired"}`, 410)
						return
					}
					if mode == "no-evidence" {
						fmt.Fprint(w, `{"status":"RUNNING"}`)
						return
					}
					if mode == "detail" || mode == "rejected-update" || mode == "cancel" {
						<-r.Context().Done()
						return
					}
					select {
					case <-detailStarted:
					case <-r.Context().Done():
						return
					}
					ready.Store(true)
					fmt.Fprint(w, `{"status":"DONE"}`)
				default:
					close(detailStarted)
					if mode == "no-evidence" {
						fmt.Fprint(w, `{"state":"PENDING"}`)
						return
					}
					if mode == "operation" || mode == "cancel" {
						<-r.Context().Done()
						return
					}
					select {
					case <-opStarted:
					case <-r.Context().Done():
						return
					}
					ready.Store(true)
					fmt.Fprint(w, `{"id":"db-1","state":"RUNNING"}`)
				}
			}))
			defer srv.Close()
			graph := admissionGraph(t, `{"id":inbox["id"]}`)
			if mode == "expired-operation" {
				wirings := append([]omnisdk.Wiring(nil), graph.Wirings()[:2]...)
				wirings = append(wirings, omnisdk.NewWiringWithTransform("update", graph.Wirings()[2].Inbound(), dsl.Transform{Type: "starlark", Body: `{"id":inbox["id"]}`, Required: []string{"id"}, Admission: &dsl.Program{Type: "starlark", Body: `edges["operation"].done and edges["operation"].error != None and edges["detail"].present and edges["detail"].value == "RUNNING"`}}, "id"))
				var err error
				graph, err = omnisdk.NewGraph(graph.Nodes(), wirings)
				if err != nil {
					t.Fatal(err)
				}
			}
			state := t.TempDir()
			args := omnisdk.Args{Endpoint: srv.URL, IACRollback: true}
			prepared, err := omnisdk.ConvergeGraph(registry, "coverage", state, "run", graph, []omnisdk.Managed{omnisdk.NewManaged("db", []string{"create", "update"}, "id", "id")}, args)
			if err != nil {
				t.Fatal(err)
			}
			timeout := 3 * time.Second
			if mode == "cancel" {
				timeout = 150 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			rows, err := prepared.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var output []omnisdk.Row
			for rows.Next() {
				output = append(output, rows.Row())
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			failed := strings.Contains(fmt.Sprint(output), "status:failed")
			if creates.Load() != 1 {
				t.Fatalf("create calls=%d output=%v", creates.Load(), output)
			}
			expectedFailure := mode == "rejected-update" || mode == "no-evidence" || mode == "cancel"
			if failed != expectedFailure {
				t.Fatalf("failure=%v output=%v", failed, output)
			}
			wantUpdates := int32(1)
			if mode == "no-evidence" || mode == "cancel" {
				wantUpdates = 0
			}
			if updates.Load() != wantUpdates {
				t.Fatalf("updates=%d want=%d output=%v", updates.Load(), wantUpdates, output)
			}
			if mode == "no-evidence" || mode == "rejected-update" {
				if deletes.Load() != 1 {
					t.Fatalf("failed run did not compensate known create: %v", output)
				}
			} else {
				// A fresh caller context can clean up known ownership after cancellation.
				cleanup, err := omnisdk.Prepare(registry, omnisdk.Destroy(), omnisdk.NewLedger("coverage", state, "cleanup"), args)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range drain(t, cleanup) {
					if row["status"] != "destroyed" {
						t.Fatalf("cleanup=%v", row)
					}
				}
				if deletes.Load() != 1 {
					t.Fatalf("cleanup deletes=%d", deletes.Load())
				}
			}
		})
	}
}
