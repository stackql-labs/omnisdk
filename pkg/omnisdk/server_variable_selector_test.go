package omnisdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/docparse/stackqldoc"
	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

func selectorRegistry(t *testing.T, spec omnisdk.ServerVariableSpec) (string, string) {
	t.Helper()
	root := t.TempDir()
	version := filepath.Join(root, "selector", "v00.00.00000")
	if err := os.MkdirAll(filepath.Join(version, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	provider := map[string]any{"id": "selector", "name": "selector", "version": "v00.00.00000", "providerServices": map[string]any{"things": map[string]any{"id": "things:v00.00.00000", "name": "things", "preferred": true, "service": map[string]any{"$ref": "selector/v00.00.00000/services/things.yaml"}}}, "config": map[string]any{"auth": map[string]any{"type": "oauth2", "grant_type": "client_credentials", "token_url": "http://{account_host}/oidc/accounts/{{ .__env__SELECTOR_TEST_ACCOUNT }}/v1/token", "variables": map[string]any{"account_host": spec, "documentation_only": omnisdk.ServerVariableSpec{Default: "ignored.example"}}}}}
	doc := map[string]any{"openapi": "3.0.0", "info": map[string]any{"title": "things", "version": "v1"}, "servers": []any{map[string]any{"url": "http://{account_host}", "variables": map[string]any{"account_host": spec}}}, "paths": map[string]any{"/items": map[string]any{"get": map[string]any{"operationId": "list_items", "responses": map[string]any{"200": map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}}}}}}}}}, "components": map[string]any{"x-stackQL-resources": map[string]any{"items": map[string]any{"name": "items", "methods": map[string]any{"list": map[string]any{"operation": map[string]any{"$ref": "#/paths/~1items/get"}, "response": map[string]any{"mediaType": "application/json", "openAPIDocKey": "200"}}}, "sqlVerbs": map[string]any{"select": []any{map[string]any{"$ref": "#/components/x-stackQL-resources/items/methods/list"}}}}}}}
	for name, value := range map[string]any{"provider.yaml": provider, "services/things.yaml": doc} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(version, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root, version
}

func TestSelectedAPIAndTokenEndpoint(t *testing.T) {
	for _, name := range []string{"default", "selector", "direct", "runtime-token-url"} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			calls := []string{}
			handler := func(label string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					calls = append(calls, label+" "+r.Method+" "+r.URL.Path)
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodPost {
						if err := r.ParseForm(); err != nil {
							t.Error(err)
						}
						if r.Form.Get("client_id") != "client" || r.Form.Get("client_secret") != "secret" || r.Form.Get("grant_type") != "client_credentials" {
							t.Error("incorrect token grant")
						}
						fmt.Fprint(w, `{"access_token":"selected-token","token_type":"Bearer","expires_in":3600}`)
					} else {
						if r.Header.Get("Authorization") != "Bearer selected-token" {
							t.Error("selected API received no token")
						}
						fmt.Fprint(w, `[{"id":"selected"}]`)
					}
				}
			}
			aws := httptest.NewServer(handler("aws"))
			defer aws.Close()
			azure := httptest.NewServer(handler("azure"))
			defer azure.Close()
			spec := omnisdk.ServerVariableSpec{Default: "ignored.example", Environment: "SELECTOR_TEST_DIRECT", Selector: &omnisdk.EnvVarSelector{EnvVar: "SELECTOR_TEST_CLOUD", Default: "aws", Values: map[string]string{"aws": strings.TrimPrefix(aws.URL, "http://"), "azure": strings.TrimPrefix(azure.URL, "http://")}}}
			t.Setenv("SELECTOR_TEST_ACCOUNT", "account-1")
			t.Setenv("SELECTOR_TEST_CLOUD", "")
			t.Setenv("SELECTOR_TEST_DIRECT", "")
			expectedAPI, expectedToken := "aws", "aws"
			auth := &omnisdk.Auth{Type: "client_credentials", ClientID: "client", ClientSecret: "secret"}
			if name == "selector" {
				t.Setenv("SELECTOR_TEST_CLOUD", " azure ")
				expectedAPI, expectedToken = "azure", "azure"
			}
			if name == "direct" {
				t.Setenv("SELECTOR_TEST_DIRECT", " "+strings.TrimPrefix(azure.URL, "http://")+"/// ")
				t.Setenv("SELECTOR_TEST_CLOUD", "invalid")
				expectedAPI, expectedToken = "azure", "azure"
			}
			root, version := selectorRegistry(t, spec)
			selected := ""
			if name == "runtime-token-url" {
				t.Setenv("SELECTOR_TEST_CLOUD", "invalid")
				auth.TokenURL = azure.URL + "/custom-token"
				selected = strings.TrimPrefix(aws.URL, "http://")
				expectedToken = "azure"
			} else {
				var ok bool
				var err error
				selected, ok, err = omnisdk.NewServerVariableResolver(spec).Resolve("account_host", os.LookupEnv)
				if err != nil || !ok {
					t.Fatalf("resolution=%v %v", ok, err)
				}
			}
			plan, err := omnisdk.NewSelectFromCatalog(root, "stackql_unstable_selector.things.items", omnisdk.Args{Params: map[string]string{"account_host": selected}, AuthByProvider: map[string]*omnisdk.Auth{"selector": auth}})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := plan.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if !rows.Next() || rows.Row()["id"] != "selected" {
				t.Fatalf("missing row: %v", rows.Err())
			}
			for rows.Next() {
			}
			if rows.Err() != nil {
				t.Fatal(rows.Err())
			}
			mu.Lock()
			observed := append([]string(nil), calls...)
			mu.Unlock()
			tokenPath := "/oidc/accounts/account-1/v1/token"
			if name == "runtime-token-url" {
				tokenPath = "/custom-token"
			}
			if len(observed) != 2 || observed[0] != expectedToken+" POST "+tokenPath || observed[1] != expectedAPI+" GET /items" {
				t.Fatalf("calls=%v", observed)
			}
			catalogue, err := stackqldoc.Open(os.DirFS(version))
			if err != nil {
				t.Fatal(err)
			}
			defaults := catalogue.Provider().(aot.AuthConfigured).AuthDefaults()
			if _, exists := defaults.TokenURLVariables["documentation_only"]; exists {
				t.Fatal("OpenAPI default opted into auth resolution")
			}
			defaults.TokenURLVariables["account_host"].Selector.Values["aws"] = "corrupted"
			if catalogue.Provider().(aot.AuthConfigured).AuthDefaults().TokenURLVariables["account_host"].Selector.Values["aws"] == "corrupted" {
				t.Fatal("mutable auth metadata escaped")
			}
			methods, err := omnisdk.DocMethods(version, "selector", "things", "items")
			if err != nil {
				t.Fatal(err)
			}
			if methods[0].ServerVariables["account_host"].Selector.Default != "aws" {
				t.Fatal("SHOW METHODS selector metadata missing")
			}
		})
	}
}

func TestTokenSelectorFailsBeforeAnyRequest(t *testing.T) {
	for _, name := range []string{"unknown", "unresolved", "invalid-default"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SELECTOR_TEST_CLOUD", "")
			t.Setenv("SELECTOR_TEST_DIRECT", "")
			t.Setenv("SELECTOR_TEST_ACCOUNT", "account")
			spec := omnisdk.ServerVariableSpec{Environment: "SELECTOR_TEST_DIRECT", Selector: &omnisdk.EnvVarSelector{EnvVar: "SELECTOR_TEST_CLOUD", Values: map[string]string{"aws": "unused.invalid"}}}
			if name == "unknown" {
				t.Setenv("SELECTOR_TEST_CLOUD", "missing")
			}
			if name == "invalid-default" {
				spec.Selector.Default = "missing"
			}
			root, _ := selectorRegistry(t, spec)
			_, err := omnisdk.NewSelectFromCatalog(root, "stackql_unstable_selector.things.items", omnisdk.Args{Params: map[string]string{"account_host": "unused.invalid"}, AuthByProvider: map[string]*omnisdk.Auth{"selector": {Type: "client_credentials", ClientID: "client", ClientSecret: "secret"}}})
			if err == nil || !strings.Contains(err.Error(), "account_host") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

type selectorAliasTable struct {
	omnisdk.Table
	address string
}

func (table selectorAliasTable) Address() string { return table.address }

func TestResolvedSQLScopeOverridesAliasDefaults(t *testing.T) {
	api := func(id string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"id":%q}]`, id)
		}))
	}
	fallback := api("fallback")
	defer fallback.Close()
	explicit := api("explicit")
	defer explicit.Close()
	host := func(server *httptest.Server) string { return strings.TrimPrefix(server.URL, "http://") }
	root, _ := selectorRegistry(t, omnisdk.ServerVariableSpec{Default: "ignored"})
	described, err := omnisdk.DescribeTable(root, "stackql_unstable_selector.things.items")
	if err != nil {
		t.Fatal(err)
	}
	address := "tenant.things.items"
	table := selectorAliasTable{Table: described, address: address}
	for _, fanout := range []bool{false, true} {
		t.Run(fmt.Sprint("fanout=", fanout), func(t *testing.T) {
			var predicate query.Predicate = query.NewEq(query.NewColumn("", "account_host"), query.NewLiteral(host(explicit)))
			expected := []string{"explicit"}
			if fanout {
				predicate = query.NewIn(query.NewColumn("", "account_host"), query.NewCollection(query.NewLiteral(host(fallback)), query.NewLiteral(host(explicit))))
				expected = []string{"explicit", "fallback"}
			}
			statement, err := query.New([]query.Join{query.NewJoin(query.NewResource("i", address), query.Base)}, []query.Predicate{predicate}, []query.Output{query.NewOutput("id", query.NewColumn("i", "id"))})
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := omnisdk.Resolve(statement, map[string]omnisdk.Table{"i": table})
			if err != nil {
				t.Fatal(err)
			}
			args := omnisdk.Args{Params: resolved.Params(), ProviderAliases: map[string]omnisdk.ProviderBinding{"tenant": {Provider: "selector", Tenancy: "account", Auth: &omnisdk.Auth{Type: "bearer", Credentials: "test-token"}, Params: map[string]string{"account_host": host(fallback)}}}}
			plan, err := omnisdk.NewGraphQuery(root, resolved.Graph(), args)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := plan.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			ids := []string{}
			for rows.Next() {
				ids = append(ids, fmt.Sprint(rows.Row()["id"]))
			}
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, expected) {
				t.Fatalf("rows=%v expected=%v", ids, expected)
			}
		})
	}
}
