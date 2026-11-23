package omnisdk_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestDesiredStateReadsThroughPost(t *testing.T) {
	registry := updateWidgetBundle(t, "")
	path := filepath.Join(registry, "widgets", "v00.00.00000", "services", "things.yaml")
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	read := `  /find-widget:
    post:
      operationId: find_widget_by_post
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [id]
              properties:
                id: {type: string}
      responses:
        '200':
          content:
            application/json:
              schema: {type: array, items: {$ref: '#/components/schemas/Widget'}}
`
	source := strings.Replace(string(doc), "components:\n", read+"components:\n", 1)
	source = strings.Replace(source, "#/paths/~1widgets/get", "#/paths/~1find-widget/post", 1)
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}

	var creates, reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/widgets":
			creates.Add(1)
			fmt.Fprint(w, `{"id":"w-1","size":"S"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/find-widget":
			var body struct {
				ID string `json:"id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID != "w-1" {
				t.Errorf("read input = %+v, err = %v", body, err)
				http.Error(w, "incorrect identifier", http.StatusBadRequest)
				return
			}
			reads.Add(1)
			fmt.Fprint(w, `[{"id":"w-1","size":"S"}]`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	state := t.TempDir()
	resource := omnisdk.NewResource("widget", "widgets", "things.widgets", []byte(`{"size":"S"}`), nil, nil, "", "", "id", "id", "")
	for _, runID := range []string{"first", "second"} {
		plan, err := omnisdk.Prepare(registry, omnisdk.DesiredState([]omnisdk.ManagedResource{resource}), omnisdk.NewLedger("demo", state, runID), omnisdk.Args{Endpoint: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		rows := drain(t, plan)
		if strings.Contains(fmt.Sprint(rows), "failed") {
			t.Fatalf("%s: %v", runID, rows)
		}
	}
	if creates, reads := creates.Load(), reads.Load(); creates != 1 || reads != 1 {
		t.Fatalf("creates=%d, reads=%d; want one create and one read on reapply", creates, reads)
	}
}
