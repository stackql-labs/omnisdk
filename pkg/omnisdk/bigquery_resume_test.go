package omnisdk_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
	"github.com/stackql-labs/omnisdk/pkg/stream"
)

type bigQueryTestTransport func(*http.Request) (*http.Response, error)

func (f bigQueryTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestBigQuerySelectStarJSONLRecoversFromPartition(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"provider.yaml": "name: bigquery\nproviderServices:\n  data:\n    service: {$ref: 'services/data.yaml'}\n",
		"services/data.yaml": `openapi: 3.0.0
servers: [{url: 'https://bigquery.test'}]
paths:
  /bigquery/v2/projects/p/datasets/d/tables/t/data:
    get:
      parameters:
        - {name: pageToken, in: query, schema: {type: string}}
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  pageToken: {type: string}
                  rows:
                    type: array
                    items:
                      type: object
                      properties:
                        f:
                          type: array
                          items:
                            type: object
                            properties:
                              v: {type: string}
components:
  x-stackQL-resources:
    tabledata:
      config:
        pagination:
          requestToken: {key: pageToken, location: query}
          responseToken: {key: '$.pageToken', location: body}
      methods:
        list:
          operation: {$ref: '#/paths/~1bigquery~1v2~1projects~1p~1datasets~1d~1tables~1t~1data/get'}
          response: {mediaType: application/json, objectKey: '$.rows'}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}

	table, err := omnisdk.DescribeTable(dir, "stackql_unstable_bigquery.data.tabledata")
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, []query.Join{query.NewJoin(query.NewResource("t", "bigquery.data.tabledata"), query.Base)},
		nil, []query.Output{query.NewOutput("", query.NewStar(""))})
	resolved, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"t": table})
	if err != nil {
		t.Fatal(err)
	}

	var tokens []string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = httpx.WithClient(ctx, &http.Client{Transport: bigQueryTestTransport(func(r *http.Request) (*http.Response, error) {
		token := r.URL.Query().Get("pageToken")
		tokens = append(tokens, token)
		var body string
		switch token {
		case "":
			body = `{"rows":[{"f":[{"v":"a"}]},{"f":[{"v":"b"}]}],"pageToken":"p2"}`
		case "p2":
			if len(tokens) == 2 {
				return nil, syscall.ENETUNREACH
			}
			body = `{"rows":[{"f":[{"v":"c"}]}]}`
		default:
			return nil, fmt.Errorf("unexpected pageToken %q", token)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	plan, err := omnisdk.NewGraphQuery(dir, resolved.Graph(), omnisdk.Args{Params: resolved.Params()})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := plan.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var output strings.Builder
	writer, err := stream.NewJSONL(&output, []string{"f"}, stream.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		if err := writer.Write(rows.Row()); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"", "p2", "p2"}; !slices.Equal(tokens, want) {
		t.Errorf("page tokens = %v, want %v", tokens, want)
	}
	want := "{\"f\":[{\"v\":\"a\"}]}\n{\"f\":[{\"v\":\"b\"}]}\n{\"f\":[{\"v\":\"c\"}]}\n"
	if output.String() != want {
		t.Errorf("JSONL = %q, want %q", output.String(), want)
	}
}
