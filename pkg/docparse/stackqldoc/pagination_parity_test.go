package stackqldoc_test

import (
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
)

func TestNativePageableDoesNotReplaceStackQLPaginationContract(t *testing.T) {
	const source = `openapi: 3.0.0
servers: [{url: 'https://example.test'}]
paths:
  /items:
    get:
      x-ms-pageable: {nextLinkName: nextLink}
      responses:
        '200':
          content:
            application/json:
              schema: {type: object}
components:
  x-stackQL-resources:
    items:
      methods:
        list:
          operation: {$ref: '#/paths/~1items/get'}
`
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "native-only", true: "explicit"}[explicit], func(t *testing.T) {
			data := source
			if explicit {
				data = strings.Replace(data, "openapi: 3.0.0", `openapi: 3.0.0
x-stackQL-config:
  pagination:
    requestToken: {location: request}
    responseToken: {key: nextLink, location: body}`, 1)
			}
			doc, err := stackqldoc.Parse([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			exchange, err := doc.Select("items")
			if err != nil {
				t.Fatal(err)
			}
			pagination := exchange.Response().Pagination()
			native, ok := pagination.(aot.NativePagination)
			if !ok || native.Native() == explicit {
				t.Fatalf("pagination provenance missing: %v", pagination)
			}
			key, location := pagination.ResponseToken()
			requestKey, requestLocation := pagination.RequestToken()
			if explicit {
				if key != "nextLink" || location != "body" || requestKey != "" || requestLocation != "request" {
					t.Fatalf("explicit contract lost: %q %q %q %q", key, location, requestKey, requestLocation)
				}
			} else if key != "nextLink" || location != "body" || requestKey != "" || requestLocation != "request" {
				t.Fatalf("native annotation changed legacy contract: %q %q %q %q", key, location, requestKey, requestLocation)
			}
		})
	}
}
