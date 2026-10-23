package docx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/awsv4"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
)

func TestRequestFallbackPreservesVendorMediaTypeAndExplicitBody(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "fallback", true: "explicit"}[explicit], func(t *testing.T) {
			want := `{"Filters":{}}`
			if explicit {
				want = `{"Filters":{"name":"caller"}}`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Content-Type"); got != "application/x-amz-json-1.1" {
					t.Errorf("Content-Type = %q", got)
				}
				var got, expected any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				_ = json.NewDecoder(strings.NewReader(want)).Decode(&expected)
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(expected)
				if string(gotJSON) != string(wantJSON) {
					t.Errorf("body = %s, want %s", gotJSON, wantJSON)
				}
				_, _ = w.Write([]byte(`{"items":[{"id":"one"}]}`))
			}))
			defer srv.Close()
			doc := []byte(`openapi: 3.0.0
servers: [{url: 'https://example.test'}]
paths:
  /items:
    post:
      requestBody:
        content:
          application/x-amz-json-1.1:
            schema: {type: object}
      responses:
        '200':
          content:
            application/json:
              schema: {type: object}
components:
  x-stackQL-resources:
    items:
      methods:
        select:
          request: {mediaType: application/x-amz-json-1.1, default: '{"Filters":{}}'}
          operation: {$ref: '#/paths/~1items/post'}
          response: {mediaType: application/json, objectKey: '$.items'}
`)
			opts := []docx.Option{docx.WithBaseURL(srv.URL)}
			if explicit {
				opts = append(opts, docx.WithBody([]byte(want)))
			}
			p, err := docx.SelectPlan(doc, "items", nil, registry(t), opts...)
			if err != nil {
				t.Fatal(err)
			}
			if rows := drain(t, p); len(rows) != 1 {
				t.Fatalf("rows = %v", rows)
			}
		})
	}
}

func TestDeclaredHeaderDefaultsAndJSONBodyInputs(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "override"}[override], func(t *testing.T) {
			want := "CloudApiService.ListResources"
			if override {
				want = "custom.target"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.RawQuery != "" || r.Header.Get("X-Amz-Target") != want {
					t.Errorf("request method/target = %s/%s", r.Method, r.Header.Get("X-Amz-Target"))
				}
				wantAccept := "application/json"
				if override {
					wantAccept = "application/custom"
				}
				if r.Header.Get("Accept") != wantAccept {
					t.Errorf("Accept = %s, want %s", r.Header.Get("Accept"), wantAccept)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				expectedType := "AWS::S3::Bucket"
				if override {
					expectedType = "AWS::Raw"
				}
				if body["TypeName"] != expectedType {
					t.Errorf("body = %v", body)
				}
				if !override && body["ClientToken"] != "from-base" {
					t.Errorf("base default absent: %v", body)
				}
				if override {
					if _, present := body["ClientToken"]; present {
						t.Errorf("explicit raw body was merged: %v", body)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"items":[{"id":"one"}]}`))
			}))
			defer srv.Close()
			doc := []byte(`openapi: 3.0.0
servers: [{url: 'https://example.test'}]
x-stackQL-config:
  requestTranslate: {algorithm: drop_double_underscore_params}
paths:
  /?__X-Amz-Target=CloudApiService.ListResources:
    post:
      parameters:
        - name: accept
          in: header
          schema: {type: string}
        - name: X-Amz-Target
          in: header
          schema: {type: string, default: CloudApiService.ListResources}
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                TypeName: {type: string}
                ClientToken: {type: string}
      responses:
        '200':
          content:
            application/json:
              schema: {type: object}
components:
  x-stackQL-resources:
    items:
      methods:
        select:
          request: {base: '{"TypeName":"AWS::Default","ClientToken":"from-base"}'}
          operation: {$ref: '#/paths/~1?__X-Amz-Target=CloudApiService.ListResources/post'}
          response: {mediaType: application/json, objectKey: '$.items'}
`)
			inputs := map[string]any{"TypeName": "AWS::S3::Bucket"}
			if override {
				inputs["accept"] = "application/custom"
				inputs["X-Amz-Target"] = want
				delete(inputs, "TypeName")
			}
			opts := []docx.Option{docx.WithBaseURL(srv.URL)}
			if override {
				opts = append(opts, docx.WithBody([]byte(`{"TypeName":"AWS::Raw"}`)))
			}
			p, err := docx.SelectPlan(doc, "items", inputs, registry(t), opts...)
			if err != nil {
				t.Fatal(err)
			}
			if rows := drain(t, p); len(rows) != 1 {
				t.Fatalf("rows = %v", rows)
			}
		})
	}
}

func TestSigningDistinguishesDeclaredAndGeneratedAccept(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(map[bool]string{false: "generated", true: "declared"}[declared], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept") != "application/json" {
					t.Errorf("Accept=%q", r.Header.Get("Accept"))
				}
				signed := strings.Contains(r.Header.Get("Authorization"), "SignedHeaders=accept;")
				if signed != declared {
					t.Errorf("declared=%v authorization=%s", declared, r.Header.Get("Authorization"))
				}
				_, _ = w.Write([]byte(`{"items":[{"id":"one"}]}`))
			}))
			defer srv.Close()
			header := ""
			if declared {
				header = "      parameters: [{name: Accept, in: header, schema: {type: string, default: application/json}}]\n"
			}
			document := []byte(`openapi: 3.0.0
servers: [{url: 'https://example.test'}]
security: [{sig: []}]
paths:
  /items:
    get:
` + header + `      responses:
        '200':
          content:
            application/json:
              schema: {type: object}
components:
  securitySchemes:
    sig: {type: apiKey, in: header, name: Authorization, x-amazon-apigateway-authtype: awsSigv4}
  x-stackQL-resources:
    items:
      methods:
        select:
          operation: {$ref: '#/paths/~1items/get'}
          response: {mediaType: application/json, objectKey: items}
`)
			p, err := docx.SelectPlan(document, "items", map[string]any{"region": "ap-southeast-1"}, registry(t), docx.WithBaseURL(srv.URL), docx.WithAWSCredentials(awsv4.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}))
			if err != nil {
				t.Fatal(err)
			}
			if rows := drain(t, p); len(rows) != 1 {
				t.Fatalf("rows=%v", rows)
			}
		})
	}
}
