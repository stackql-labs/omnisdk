package omnisdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/rowcache"
	"github.com/stackql-labs/omnisdk/public/authorization"
	"github.com/stackql-labs/omnisdk/public/principal"
	"github.com/stackql-labs/omnisdk/public/secrets"
)

func tenancyRegistry(t *testing.T, endpoint string) string {
	t.Helper()
	root := t.TempDir()
	provider := `{"name":"demo","providerServices":{"things":{"service":{"$ref":"service.yaml"}}},"config":{"auth":{"type":"bearer"}}}`
	doc := fmt.Sprintf(`{"openapi":"3.0.0","servers":[{"url":%q}],"paths":{"/items":{"get":{"operationId":"listItems","parameters":[{"name":"region","in":"query","schema":{"type":"string"}}],"responses":{"200":{"content":{"application/json":{"schema":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"}}}}}}}}}}},"components":{"x-stackQL-resources":{"items":{"methods":{"list":{"operation":{"$ref":"#/paths/~1items/get"},"response":{"mediaType":"application/json"}}},"sqlVerbs":{"select":[{"$ref":"#/components/x-stackQL-resources/items/methods/list"}]}}}}}`, endpoint)
	for name, text := range map[string]string{"provider.yaml": provider, "service.yaml": doc} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func consumeTenantPlan(t *testing.T, registry, address string, args omnisdk.Args) {
	t.Helper()
	plan, err := omnisdk.NewSelectFromCatalog(registry, address, args)
	if err != nil {
		t.Error(err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := plan.Open(ctx)
	if err != nil {
		t.Error(err)
		return
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Error(err)
	}
}
func TestClientSessionCredentialsSnapshotsAndSecretStore(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Header.Get("Authorization")+"/"+r.URL.Query().Get("region"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"one"}]`)
	}))
	defer server.Close()
	root := tenancyRegistry(t, server.URL)
	store, err := secrets.New(secrets.Config{Path: filepath.Join(t.TempDir(), "secrets")})
	if err != nil {
		t.Fatal(err)
	}
	authJSON, _ := json.Marshal(omnisdk.Auth{Type: "bearer", Credentials: "second"})
	if err := store.Set(context.Background(), "account-b", authJSON); err != nil {
		t.Fatal(err)
	}
	session, err := omnisdk.NewClientSession(omnisdk.Args{}, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.SetProvider("east", omnisdk.ProviderBinding{Provider: "demo", Tenancy: "account-a", Auth: &omnisdk.Auth{Type: "bearer", Credentials: "first"}, Params: map[string]string{"region": "east"}}); err != nil {
		t.Fatal(err)
	}
	if err := session.SetProvider("west", omnisdk.ProviderBinding{Provider: "demo", Tenancy: "account-b", AuthSecret: "account-b", Params: map[string]string{"region": "west"}}); err != nil {
		t.Fatal(err)
	}
	old, err := session.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.PatchAuth("east", map[string]any{"credentials": "changed"}); err != nil {
		t.Fatal(err)
	}
	if err := session.PatchAuth("east", map[string]any{"scopes": 42}); err == nil {
		t.Fatal("accepted invalid auth type")
	}
	if err := session.PatchAuth("east", map[string]any{"typo": "secret"}); err == nil {
		t.Fatal("accepted unknown auth field")
	}
	current, err := session.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	consumeTenantPlan(t, root, "east.things.items", old)
	consumeTenantPlan(t, root, "east.things.items", current)
	consumeTenantPlan(t, root, "west.things.items", current)
	if err := session.UnsetAuth("east", "credentials"); err != nil {
		t.Fatal(err)
	}
	unset, err := session.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if unset.ProviderAliases["east"].Auth.Credentials != "" {
		t.Fatal("UNSET did not clear field")
	}
	if err := session.UnsetAuth("east"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Snapshot(context.Background()); err == nil {
		t.Fatal("unset auth silently fell back")
	}
	if err := session.UnsetProvider("east"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(requests) != "[Bearer first/east Bearer changed/east Bearer second/west]" {
		t.Fatal(requests)
	}
}
func TestTenancyAdmissionSharedAcrossQueriesAndIndependentAcrossAccounts(t *testing.T) {
	var mu sync.Mutex
	active, maxima := map[string]int{}, map[string]int{}
	arrived := make(chan string, 20)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := r.Header.Get("Authorization")
		mu.Lock()
		active[account]++
		if active[account] > maxima[account] {
			maxima[account] = active[account]
		}
		mu.Unlock()
		arrived <- account
		select {
		case <-release:
		case <-r.Context().Done():
		}
		mu.Lock()
		active[account]--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"one"}]`)
	}))
	defer server.Close()
	root := tenancyRegistry(t, server.URL)
	session, err := omnisdk.NewClientSession(omnisdk.Args{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := session.SetProvider(name, omnisdk.ProviderBinding{Provider: "demo", Tenancy: name, Auth: &omnisdk.Auth{Type: "bearer", Credentials: name}, Tuning: omnisdk.ProviderTuning{MaxConcurrent: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range 4 {
		for _, name := range []string{"a", "b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				args, err := session.Snapshot(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				consumeTenantPlan(t, root, name+".things.items", args)
			}()
		}
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case account := <-arrived:
			seen[account] = true
		case <-time.After(3 * time.Second):
			close(release)
			wg.Wait()
			t.Fatal("independent tenancies blocked each other")
		}
	}
	close(release)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if maxima["Bearer a"] != 1 || maxima["Bearer b"] != 1 {
		t.Fatal(maxima)
	}
}
func TestProviderAliasGraphUsesSeparateCredentials(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"one"}]`)
	}))
	defer server.Close()
	root := tenancyRegistry(t, server.URL)
	args := omnisdk.Args{ProviderAliases: map[string]omnisdk.ProviderBinding{}}
	for _, name := range []string{"a", "b"} {
		args.ProviderAliases[name] = omnisdk.ProviderBinding{Provider: "demo", Tenancy: name, Auth: &omnisdk.Auth{Type: "bearer", Credentials: name}}
	}
	graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("one", "a.things.items", nil), omnisdk.NewNode("two", "b.things.items", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := omnisdk.NewGraphQuery(root, graph, args)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !seen["Bearer a"] || !seen["Bearer b"] {
		t.Fatal(seen)
	}
}
func BenchmarkClientSessionSnapshot(b *testing.B) {
	s, err := omnisdk.NewClientSession(omnisdk.Args{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	s.SetProvider("account", omnisdk.ProviderBinding{Provider: "aws", Tenancy: "one", Auth: &omnisdk.Auth{AccessKeyID: "key", SecretAccessKey: "secret"}})
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.Snapshot(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func TestTenancyAccessRequiresExplicitPermission(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"one"}]`)
	}))
	defer server.Close()
	root := tenancyRegistry(t, server.URL)
	directory, err := principal.NewMemoryDirectoryFromBytes([]byte(`{"principals":[{"id":"reader","identities":[{"issuer":"test","subject":"reader"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := principal.NewIdentity("test", "reader")
	for _, allowed := range []bool{false, true} {
		rules := []authorization.Rule{{Subject: "user:reader", Resource: "operation/demo/list/listItems", Action: "execute"}, {Subject: "user:reader", Resource: "credential/demo", Action: "use"}}
		if allowed {
			rules = append(rules, authorization.Rule{Subject: "user:reader", Resource: "tenancy/demo/account", Action: "use"})
		}
		authz, _ := authorization.NewCasbin(rules)
		caller, err := authorization.NewSession(context.Background(), identity, directory, authz)
		if err != nil {
			t.Fatal(err)
		}
		args := omnisdk.Args{Caller: caller, RequireCaller: true, ProviderAliases: map[string]omnisdk.ProviderBinding{"account": {Provider: "demo", Tenancy: "account", Auth: &omnisdk.Auth{Type: "bearer", Credentials: "account-token"}}}}
		plan, err := omnisdk.NewSelectFromCatalog(root, "account.things.items", args)
		if err != nil {
			t.Fatal(err)
		}
		before := calls.Load()
		rows, err := plan.Open(context.Background())
		if !allowed {
			if !errors.Is(err, authorization.ErrDenied) || calls.Load() != before {
				t.Fatalf("err=%v calls=%d", err, calls.Load())
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if calls.Load() != before+1 {
			t.Fatal("allowed scope did not execute")
		}
	}
}
func TestSessionConcurrentMutationAndSnapshots(t *testing.T) {
	s, err := omnisdk.NewClientSession(omnisdk.Args{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProvider("account", omnisdk.ProviderBinding{Provider: "demo", Tenancy: "account", Auth: &omnisdk.Auth{Type: "bearer", Credentials: "initial"}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				if err := s.PatchAuth("account", map[string]any{"credentials": fmt.Sprint(i)}); err != nil {
					t.Error(err)
					return
				}
				args, err := s.Snapshot(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				args.ProviderAliases["account"].Auth.Credentials = "caller-owned"
			}
		}()
	}
	wg.Wait()
	args, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if args.ProviderAliases["account"].Auth.Credentials == "caller-owned" {
		t.Fatal("snapshot modified session")
	}
}
func TestTenancyCacheIsolation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"id":%q}]`, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	root := tenancyRegistry(t, server.URL)
	cache, err := rowcache.NewMemory(context.Background(), rowcache.Config{TTL: time.Minute}, rowcache.MemoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	args := omnisdk.Args{ReadCache: cache, ReadCachePartition: "caller", ProviderAliases: map[string]omnisdk.ProviderBinding{}}
	for _, name := range []string{"a", "b"} {
		args.ProviderAliases[name] = omnisdk.ProviderBinding{Provider: "demo", Tenancy: name, Auth: &omnisdk.Auth{Type: "bearer", Credentials: name}}
	}
	for _, name := range []string{"a", "b", "a", "b"} {
		plan, err := omnisdk.NewSelectFromCatalog(root, name+".things.items", args)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := plan.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			if rows.Row()["id"] != "Bearer "+name {
				t.Fatal("cross-tenancy cache leak")
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	if calls.Load() != 2 {
		t.Fatalf("wire calls=%d, want two cache misses", calls.Load())
	}
	args.ReadCachePartition = ""
	if _, err := omnisdk.NewSelectFromCatalog(root, "a.things.items", args); err == nil {
		t.Fatal("inferred caller cache partition")
	}
}

func TestSessionSelectiveSnapshotsAndCacheGeneration(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"id":%q}]`, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	root := tenancyRegistry(t, server.URL)
	cache, err := rowcache.NewMemory(context.Background(), rowcache.Config{TTL: time.Minute}, rowcache.MemoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	s, err := omnisdk.NewClientSession(omnisdk.Args{ReadCache: cache, ReadCachePartition: "caller"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProvider("one", omnisdk.ProviderBinding{Provider: "demo", Tenancy: "one", Auth: &omnisdk.Auth{Type: "bearer", Credentials: "first"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProvider("unrelated", omnisdk.ProviderBinding{Provider: "demo", Tenancy: "unrelated", AuthSecret: "unavailable"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(context.Background()); err == nil {
		t.Fatal("ignored unresolved selected secret")
	}
	if _, err := s.Snapshot(context.Background(), "unknown"); err == nil {
		t.Fatal("ignored unknown selected alias")
	}
	read := func(want string) {
		args, err := s.Snapshot(context.Background(), "one")
		if err != nil {
			t.Fatal(err)
		}
		plan, err := omnisdk.NewSelectFromCatalog(root, "one.things.items", args)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := plan.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			if rows.Row()["id"] != want {
				t.Fatal("session mutation reused earlier credential cache")
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	read("Bearer first")
	read("Bearer first")
	if err := s.PatchAuth("one", map[string]any{"credentials": "second"}); err != nil {
		t.Fatal(err)
	}
	read("Bearer second")
	read("Bearer second")
	if calls.Load() != 2 {
		t.Fatalf("requests=%d", calls.Load())
	}
	if err := s.SetProvider("conflict", omnisdk.ProviderBinding{Provider: "demo", Tenancy: "one", Auth: &omnisdk.Auth{}, Tuning: omnisdk.ProviderTuning{MaxConcurrent: 5}}); err == nil {
		t.Fatal("allowed conflicting limit on an unselected alias")
	}
}
func TestAWSProviderAliasesSignWithTheirOwnKeys(t *testing.T) {
	requireCorpus(t)
	var mu sync.Mutex
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		if strings.Contains(auth, "Credential=EAST/") && strings.Contains(auth, "/us-east-1/") {
			seen["east"] = true
		}
		if strings.Contains(auth, "Credential=WEST/") && strings.Contains(auth, "/ap-southeast-2/") {
			seen["west"] = true
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<ListAllMyBucketsResult><Buckets><Bucket><Name>one</Name></Bucket></Buckets></ListAllMyBucketsResult>`)
	}))
	defer server.Close()
	args := omnisdk.Args{Endpoint: server.URL, ProviderAliases: map[string]omnisdk.ProviderBinding{
		"east": {Provider: "aws", Tenancy: "east-account", Auth: &omnisdk.Auth{AccessKeyID: "EAST", SecretAccessKey: "east-secret"}, Params: map[string]string{"region": "us-east-1"}},
		"west": {Provider: "aws", Tenancy: "west-account", Auth: &omnisdk.Auth{AccessKeyID: "WEST", SecretAccessKey: "west-secret"}, Params: map[string]string{"region": "ap-southeast-2"}},
	}}
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("east", "east.s3.buckets", nil), omnisdk.NewNode("west", "west.s3.buckets", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := omnisdk.NewGraphQuery(corpus, g, args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := p.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !seen["east"] || !seen["west"] {
		t.Fatal("account credentials or signing region crossed", seen)
	}
}

func TestSessionExternalSecretRotationInvalidatesCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"id":%q}]`, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	root := tenancyRegistry(t, server.URL)
	cache, err := rowcache.NewMemory(context.Background(), rowcache.Config{TTL: time.Minute}, rowcache.MemoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	store, err := secrets.New(secrets.Config{Path: filepath.Join(t.TempDir(), "auth")})
	if err != nil {
		t.Fatal(err)
	}
	s, err := omnisdk.NewClientSession(omnisdk.Args{ReadCache: cache, ReadCachePartition: "caller"}, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProvider("one", omnisdk.ProviderBinding{Provider: "demo", Tenancy: "one", AuthSecret: "auth"}); err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{"first", "second"} {
		raw, _ := json.Marshal(omnisdk.Auth{Type: "bearer", Credentials: credential})
		if err := store.Set(context.Background(), "auth", raw); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			args, err := s.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			p, err := omnisdk.NewSelectFromCatalog(root, "one.things.items", args)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := p.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				if rows.Row()["id"] != "Bearer "+credential {
					t.Fatal("stored credential rotation leaked cached rows")
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("wire requests=%d, want two cache misses", calls.Load())
	}
}
