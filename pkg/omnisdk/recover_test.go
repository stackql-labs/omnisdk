package omnisdk_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestRecoverPendingCreateThenResumeWithoutDuplicatingCommittedResources(t *testing.T) {
	for _, outcome := range []string{"absent", "applied"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			store := &widgetStore{items: map[string]map[string]any{}}
			backend := store.serve(t)
			defer backend.Close()
			target, _ := url.Parse(backend.URL)
			proxy := httputil.NewSingleHostReverseProxy(target)
			var dropped atomic.Bool
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && dropped.CompareAndSwap(false, true) {
					if outcome == "applied" {
						request, err := http.NewRequestWithContext(r.Context(), r.Method, backend.URL+r.URL.RequestURI(), r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						request.Header = r.Header.Clone()
						response, err := http.DefaultClient.Do(request)
						if err != nil {
							t.Error(err)
							return
						}
						_, _ = io.Copy(io.Discard, response.Body)
						response.Body.Close()
					}
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					connection.Close()
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer front.Close()
			state := t.TempDir()
			apply := func(spec, endpoint, run string) []omnisdk.Row {
				t.Helper()
				resources, err := omnisdk.ParseResources([]byte(spec))
				if err != nil {
					t.Fatal(err)
				}
				plan, err := omnisdk.Prepare(authRegistry, omnisdk.DesiredState(resources), omnisdk.NewLedger("demo", state, run), omnisdk.Args{Endpoint: endpoint})
				if err != nil {
					t.Fatal(err)
				}
				return readPlan(t, plan)
			}
			stable := `{"key":"stable","provider":"widgets","address":"things.widgets","identity":"id","addressed_by":"id","desired":{"size":"S","color":"red"}}`
			created := apply("["+stable+"]", backend.URL, "initial")
			if len(created) != 1 || created[0]["identity"] != "w-1" {
				t.Fatal(created)
			}
			spec := "[" + stable + `,{"key":"new","provider":"widgets","address":"things.widgets","identity":"id","addressed_by":"id","desired":{"size":"S","color":"blue"}}]`
			failed := apply(spec, front.URL, "interrupted")
			if failed[len(failed)-1]["error"] == nil {
				t.Fatal("expected unknown outcome", failed)
			}
			rows, err := omnisdk.ReadLedger(ctx, "demo", state)
			if err != nil {
				t.Fatal(err)
			}
			version := ""
			for _, row := range rows {
				if row["resource_key"] == "new" {
					version = fmt.Sprint(row["version"])
					if row["phase"] != "pending" {
						t.Fatal(row)
					}
				}
			}
			identity := ""
			if outcome == "applied" {
				identity = "w-2"
			}
			store.took()
			recovery, err := omnisdk.Prepare("", omnisdk.Recover("new", outcome, identity, version), omnisdk.NewLedger("demo", state, "recover"), omnisdk.Args{})
			if err != nil {
				t.Fatal(err)
			}
			recovered := readPlan(t, recovery)
			if len(recovered) != 1 || recovered[0]["status"] != "recovered_"+outcome {
				t.Fatal(recovered)
			}
			if calls := store.took(); len(calls) != 0 {
				t.Fatal("recovery called provider", calls)
			}
			resumed := apply(spec, front.URL, "resumed")
			for _, row := range resumed {
				if row["error"] != nil {
					t.Fatal(row)
				}
			}
			store.mu.Lock()
			if len(store.items) != 2 {
				t.Fatal("duplicated or lost resources", store.items)
			}
			store.mu.Unlock()
			for _, row := range resumed {
				if row["key"] == "demo/stable" && row["identity"] != "w-1" {
					t.Fatal("replaced committed resource", row)
				}
			}
		})
	}
}

func readPlan(t *testing.T, plan omnisdk.Plan) []omnisdk.Row {
	t.Helper()
	rows, err := plan.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []omnisdk.Row
	for rows.Next() {
		result = append(result, rows.Row())
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
