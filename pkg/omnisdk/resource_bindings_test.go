package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestResourceBindingsResolveBeforeEffects(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Error("request made before binding validation")
	}))
	defer server.Close()
	resources, err := omnisdk.ParseResources([]byte(`[
 {"key":"first","provider":"widgets","address":"things.widgets","identity":"id","addressed_by":"id","desired":{"size":"S","color":"red"}},
 {"key":"second","provider":"widgets","address":"things.widgets","identity":"id","addressed_by":"id","desired":{"size":{"from":"input","attribute":"missing"}}}
 ]`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = omnisdk.Prepare(authRegistry, omnisdk.DesiredState(resources), omnisdk.NewLedger("bindings", t.TempDir(), "run"), omnisdk.Args{Endpoint: server.URL})
	if err == nil || !strings.Contains(err.Error(), `unresolved input binding "missing"`) {
		t.Fatalf("error=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("effects before resolving all resources")
	}
}
func TestResourceBindingsReachTheRequest(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{}}
	server := store.serve(t)
	defer server.Close()
	resources, err := omnisdk.ParseResources([]byte(`[{"key":"widget","provider":"widgets","address":"things.widgets","identity":"id","addressed_by":"id","desired":{"size":{"from":"env","attribute":"SIZE"},"color":{"from":"self","attribute":"key"}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	for i := 0; i < 2; i++ {
		p, err := omnisdk.Prepare(authRegistry, omnisdk.DesiredState(resources), omnisdk.NewLedger("bindings", state, fmt.Sprintf("run-%d", i)), omnisdk.Args{Endpoint: server.URL, Environment: map[string]any{"SIZE": "S"}})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := p.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			if r := rows.Row(); r["error"] != nil {
				t.Fatal(r)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.items["w-1"]["color"] != "widget" || store.items["w-1"]["size"] != "S" {
		t.Fatalf("request=%v", store.items)
	}
	if strings.Join(store.calls, ",") != "create,find" {
		t.Fatalf("calls=%v", store.calls)
	}
}
