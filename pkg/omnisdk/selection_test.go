package omnisdk_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestPublicDocumentStrictSelectionIsOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	doc := []byte(`openapi: 3.0.0
servers: [{url: 'http://example.test'}]
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
                  items: {type: array, items: {type: object, properties: {name: {type: string}}}}
components:
  x-stackQL-resources:
    items:
      methods:
        list:
          operation: {$ref: '#/paths/~1items/get'}
          response: {mediaType: application/json, objectKey: items}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/items/methods/list'}]
`)
	for _, scenario := range []struct{ strict, tolerate bool }{{false, false}, {true, false}, {true, true}} {
		strict := scenario.strict
		var policy *missingKeyPolicy
		var selectionPolicy omnisdk.ResponseSelectionErrorPolicy
		if scenario.tolerate {
			policy = &missingKeyPolicy{}
			selectionPolicy = policy
		}
		plan, err := omnisdk.NewFromDoc(doc, "fixture", "items", omnisdk.Args{Endpoint: srv.URL, StrictResponseSelection: strict, ResponseSelectionErrors: selectionPolicy})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := plan.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if rows.Next() {
			t.Fatal("missing key produced a row")
		}
		err = rows.Err()
		rows.Close()
		if (err != nil) != (strict && !scenario.tolerate) {
			t.Fatalf("strict=%v error=%v", strict, err)
		}
		if strict && !scenario.tolerate && !strings.Contains(err.Error(), "unknown key items") {
			t.Fatal(err)
		}
		if scenario.tolerate && policy.key != "items" {
			t.Fatalf("missing diagnostic: %q", policy.key)
		}
	}
}

type missingKeyPolicy struct{ key string }

func (p *missingKeyPolicy) IgnoreMissingKey(key string) bool { p.key = key; return true }
