package docx_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
)

func TestDocumentRetryAttemptBounds(t *testing.T) {
	for _, attempts := range []int{1, 2, 5} {
		t.Run(fmt.Sprint(attempts), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if calls < 5 {
					w.WriteHeader(503)
					fmt.Fprint(w, `{"error":"temporary"}`)
					return
				}
				fmt.Fprint(w, `{"items":[{"ok":true}]}`)
			}))
			defer server.Close()
			doc := []byte(fmt.Sprintf(`openapi: 3.0.0
servers: [{url: 'https://example.test'}]
paths:
  /items:
    get:
      responses:
        '200':
          content:
            application/json:
              schema: {type: object}
components:
  x-stackQL-resources:
    items:
      config:
        retry:
          max_attempts: %d
          initial_delay_ms: 1
          max_delay_ms: 2
          retryable_methods: [GET]
          retryable_conditions: {status_codes: [503]}
      methods:
        select:
          operation: {$ref: '#/paths/~1items/get'}
          response: {mediaType: application/json, objectKey: '$.items'}
`, attempts))
			p, err := docx.SelectPlan(doc, "items", nil, registry(t), docx.WithBaseURL(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			rows := plan.ComposeRows(1, p).Open(ctx)
			defer rows.Close()
			got := 0
			for rows.Next(ctx) {
				got++
			}
			if calls != attempts {
				t.Fatalf("requests = %d, want %d", calls, attempts)
			}
			if attempts == 5 {
				if rows.Err() != nil || got != 1 {
					t.Fatalf("recovery rows=%d, error=%v", got, rows.Err())
				}
			} else if rows.Err() == nil {
				t.Fatal("exhausted retry budget did not report failure")
			}
		})
	}
}
