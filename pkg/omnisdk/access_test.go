package omnisdk_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/public/authorization"
	"github.com/stackql-labs/omnisdk/public/principal"
)

func TestGeneratedDependencyIsAuthorized(t *testing.T) {
	root := t.TempDir()
	doc := `openapi: 3.0.0
servers: [{url: 'http://example.test'}]
paths:
  /parents:
    get:
      operationId: listParents
      responses:
        '200':
          content:
            application/json:
              schema: {type: array, items: {type: object, properties: {id: {type: string}}}}
  /children:
    get:
      operationId: listChildren
      parameters: [{name: parent, in: query, required: true, schema: {type: string}}]
      responses:
        '200':
          content:
            application/json:
              schema: {type: array, items: {type: object, properties: {name: {type: string}}}}
components:
  x-stackQL-resources:
    parents:
      methods:
        list:
          operation: {$ref: '#/paths/~1parents/get'}
          response: {mediaType: application/json}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/parents/methods/list'}]
    children:
      methods:
        list:
          operation: {$ref: '#/paths/~1children/get'}
          response: {mediaType: application/json}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/children/methods/list'}]
`
	for name, data := range map[string]string{"provider.yaml": "name: demo\nproviderServices:\n  service:\n    service: {$ref: service.yaml}\n", "service.yaml": doc} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var children atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/parents" {
			fmt.Fprint(w, `[{"id":"p1"}]`)
		} else {
			children.Add(1)
			fmt.Fprint(w, `[{"name":"child"}]`)
		}
	}))
	defer srv.Close()
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("p", "stackql_unstable_demo.service.parents", nil), omnisdk.NewNode("c", "stackql_unstable_demo.service.children", nil)}, []omnisdk.Wiring{omnisdk.NewWiring("c", []omnisdk.Inbound{omnisdk.NewInbound("p", "id", "parent")}, "", "")})
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphQuery(root, g, omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	d, err := principal.NewMemoryDirectoryFromBytes([]byte(`{"principals":[{"id":"alice","identities":[{"issuer":"local","subject":"alice"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	i, _ := principal.NewIdentity("local", "alice")
	for _, allow := range []bool{true, false, true} {
		rules := []authorization.Rule{{Subject: "user:alice", Resource: "operation/demo/list/listParents", Action: "execute"}, {Subject: "user:alice", Resource: "credential/*", Action: "use"}}
		if allow {
			rules = append(rules, authorization.Rule{Subject: "user:alice", Resource: "operation/demo/list/listChildren", Action: "execute"})
		}
		a, err := authorization.NewCasbin(rules)
		if err != nil {
			t.Fatal(err)
		}
		s, err := authorization.NewSession(context.Background(), i, d, a)
		if err != nil {
			t.Fatal(err)
		}
		before := children.Load()
		rows, err := pl.Open(s.Context(context.Background()))
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
		if allow {
			if err != nil || children.Load() != before+1 {
				t.Fatalf("allowed child err=%v calls=%d", err, children.Load()-before)
			}
		} else {
			if !errors.Is(err, authorization.ErrDenied) || children.Load() != before {
				t.Fatalf("denied child err=%v calls=%d", err, children.Load()-before)
			}
		}
	}
}

func TestDeniedMutationDoesNotRecordIntent(t *testing.T) {
	d, err := principal.NewMemoryDirectoryFromBytes([]byte(`{"principals":[{"id":"alice","identities":[{"issuer":"local","subject":"alice"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	i, _ := principal.NewIdentity("local", "alice")
	a, err := authorization.NewCasbin(nil)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := authorization.NewSession(context.Background(), i, d, a)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	_, made, err := tryQueryWith(t, insertCarol(t), func(args *omnisdk.Args) {
		args.Caller = caller
		args.RequireCaller = true
		args.Journal = &omnisdk.Journal{State: state, RunID: "denied"}
	}, nil)
	if !errors.Is(err, authorization.ErrDenied) || !errors.Is(err, omnisdk.ErrNotAttempted) || len(made) != 0 {
		t.Fatalf("err=%v requests=%v", err, made)
	}
	raw, err := os.ReadFile(filepath.Join(state, "journal", "denied.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("denied mutation recorded an intent: %s", raw)
	}
}

func TestRequireCallerFailsBeforeExecution(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	pl, err := omnisdk.New("aws.s3.buckets.list", omnisdk.Args{RequireCaller: true, Params: map[string]string{"region": "us-east-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pl.Open(context.Background()); !errors.Is(err, authorization.ErrDenied) {
		t.Fatalf("unverified caller: %v", err)
	}
}
