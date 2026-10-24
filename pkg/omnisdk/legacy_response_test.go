package omnisdk_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestUnknownResponseProgramFallbackIsExplicit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `<items><item>one</item></items>`)
	}))
	defer server.Close()
	document := []byte(`openapi: 3.0.0
servers: [{url: 'http://example.test'}]
paths:
  /items:
    get:
      responses:
        '200': {description: ok}
components:
  x-stackQL-resources:
    items:
      methods:
        list:
          operation: {$ref: '#/paths/~1items/get'}
          response:
            mediaType: application/xml
            overrideMediaType: application/json
            transform: {type: unknown_response_version, body: '{{ . }}'}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/items/methods/list'}]
`)
	_, err := omnisdk.NewFromDoc(document, "fixture", "items", omnisdk.Args{Endpoint: server.URL})
	var unsupported omnisdk.UnsupportedProgramError
	if !errors.As(err, &unsupported) || calls.Load() != 0 {
		t.Fatalf("strict fallback dispatched unsupported program: calls=%d err=%v", calls.Load(), err)
	}
	plan, err := omnisdk.NewFromDoc(document, "fixture", "items", omnisdk.Args{Endpoint: server.URL, LegacyResponseTransforms: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := plan.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if rows.Err() == nil || !strings.Contains(rows.Err().Error(), "error processing response: invalid character '<'") || calls.Load() != 1 {
		t.Fatalf("legacy decoder fallback: calls=%d err=%v", calls.Load(), rows.Err())
	}
}
