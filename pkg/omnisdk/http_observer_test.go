package omnisdk_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

type observedHTTP struct {
	mu                         sync.Mutex
	requests, responses, pages int
	target                     string
}

func (o *observedHTTP) HTTPRequest(method, url string, body []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests++
	o.target = url
}
func (o *observedHTTP) HTTPResponse(status int, body []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.responses++
}
func (o *observedHTTP) HTTPRows(rows any) { o.mu.Lock(); defer o.mu.Unlock(); o.pages++ }

func TestDocumentObserverExcludesCredentialExchange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			fmt.Fprint(w, `{"access_token":"secret-token"}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"id":"one"}]}`)
	}))
	defer server.Close()
	observer := &observedHTTP{}
	args := omnisdk.Args{Endpoint: server.URL, HTTPObserver: observer, AuthByProvider: map[string]*omnisdk.Auth{"oauthp": {Type: "client_credentials", ClientID: "id", ClientSecret: "secret", TokenURL: server.URL + "/token"}}}
	graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("n", "stackql_unstable_oauthp.things.items", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := omnisdk.NewGraphQuery(authRegistry, graph, args)
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
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.requests != 1 || observer.responses != 1 || observer.pages != 1 || strings.Contains(observer.target, "/token") {
		t.Fatalf("observer counts=%d/%d/%d target=%s", observer.requests, observer.responses, observer.pages, observer.target)
	}
}

func TestOAuthFailureRetainsTargetAndTypedCause(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"msg":"auth failed"}`)
	}))
	defer server.Close()
	graph, _ := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("n", "stackql_unstable_oauthp.things.items", nil)}, nil)
	plan, err := omnisdk.NewGraphQuery(authRegistry, graph, omnisdk.Args{Endpoint: server.URL, AuthByProvider: map[string]*omnisdk.Auth{"oauthp": {Type: "client_credentials", ClientID: "id", ClientSecret: "secret", TokenURL: server.URL + "/token"}}})
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
	var authentication omnisdk.AuthenticationError
	var response omnisdk.HTTPStatusError
	if !errors.As(rows.Err(), &authentication) || !errors.As(rows.Err(), &response) || response.StatusCode() != 401 {
		t.Fatalf("untyped authentication failure: %v", rows.Err())
	}
	if !strings.Contains(rows.Err().Error(), server.URL+"/oauthp/items") {
		t.Fatalf("missing resource request context: %v", rows.Err())
	}
}
