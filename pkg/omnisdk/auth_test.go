package omnisdk_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

const authRegistry = "testdata/authreg"

// One query reads three providers, each authenticated its own way with its own credentials: basic
// from the environment variables its document names, a bearer token and a header key from each
// provider's entry in Auth.
func TestEachProviderAuthenticatesItsOwnWay(t *testing.T) {
	t.Setenv("TEST_BASIC_USER", "alice")
	t.Setenv("TEST_BASIC_PASS", "s3cret")
	var mu sync.Mutex
	seen := map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[strings.Split(strings.Trim(r.URL.Path, "/"), "/")[0]] = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"id":"1"}]}`)
	}))
	defer srv.Close()

	g, err := omnisdk.NewGraph([]omnisdk.Node{
		omnisdk.NewNode("b", "stackql_unstable_basicp.things.items", nil),
		omnisdk.NewNode("t", "stackql_unstable_bearp.things.items", nil),
		omnisdk.NewNode("k", "stackql_unstable_keyed.things.items", nil),
	}, nil)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(authRegistry, g, omnisdk.Args{
		Endpoint: srv.URL,
		AuthByProvider: map[string]*omnisdk.Auth{
			"stackql_unstable_bearp": {Credentials: "tok-b"},
			"keyed":                  {Credentials: "key-k"},
			"basicp":                 {},
		},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	if got := seen["basicp"].Get("Authorization"); got != basic {
		t.Errorf("basic: Authorization = %q, want %q", got, basic)
	}
	if got := seen["bearp"].Get("Authorization"); got != "Bearer tok-b" {
		t.Errorf("bearer: Authorization = %q", got)
	}
	if got := seen["keyed"].Get("x-api-key"); got != "key-k" {
		t.Errorf("custom: x-api-key = %q", got)
	}
}

// A credential is given to one provider and reaches no other: a provider with no entry of its own
// authenticates from its document's defaults, and with none there it is refused, never handed a
// neighbour's credential.
func TestCredentialReachesOnlyItsProvider(t *testing.T) {
	t.Setenv("TEST_API_KEY", "key-from-env")
	t.Setenv("TEST_BEARER_TOKEN", "")
	var mu sync.Mutex
	seen := map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[strings.Split(strings.Trim(r.URL.Path, "/"), "/")[0]] = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"id":"1"}]}`)
	}))
	defer srv.Close()
	both, err := omnisdk.NewGraph([]omnisdk.Node{
		omnisdk.NewNode("t", "stackql_unstable_bearp.things.items", nil),
		omnisdk.NewNode("k", "stackql_unstable_keyed.things.items", nil),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(authRegistry, both, omnisdk.Args{Endpoint: srv.URL,
		AuthByProvider: map[string]*omnisdk.Auth{"bearp": {Credentials: "tok-b"}}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
	if got := seen["keyed"].Get("x-api-key"); got != "key-from-env" {
		t.Errorf("keyed: x-api-key = %q, want its own document's credential", got)
	}

	// Given only the other provider's credential, bearp has none of its own and is refused.
	solo, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("t", "stackql_unstable_bearp.things.items", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = omnisdk.NewGraphSelectQuery(authRegistry, solo, omnisdk.Args{Endpoint: srv.URL,
		AuthByProvider: map[string]*omnisdk.Auth{"keyed": {Credentials: "key-k"}}})
	if err == nil || !strings.Contains(err.Error(), "bearp bearer credentials cannot be used") {
		t.Errorf("err = %v, want bearp refused rather than sent keyed's credential", err)
	}
}

// A provider whose document requires auth, with no credential anywhere, is refused before any request.
func TestMissingCredentialIsRefused(t *testing.T) {
	t.Setenv("TEST_BEARER_TOKEN", "")
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("t", "stackql_unstable_bearp.things.items", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = omnisdk.NewGraphSelectQuery(authRegistry, g, omnisdk.Args{Endpoint: "http://127.0.0.1:9"})
	if err == nil || !strings.Contains(err.Error(), "bearp bearer credentials cannot be used") {
		t.Errorf("err = %v, want the missing bearer credential named", err)
	}
}

// Columns carry the type their document declares.
func TestColumnTypesComeFromTheSchema(t *testing.T) {
	requireCorpus(t)
	tbl, err := omnisdk.DescribeTable(corpus, "stackql_unstable_google.storage.buckets")
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]omnisdk.ColumnType{}
	for _, m := range tbl.Methods() {
		for k, v := range m.ColumnTypes() {
			types[k] = v
		}
	}
	if got := types["name"]; got.Type != "string" {
		t.Errorf("name = %+v, want string", got)
	}
	if got := types["timeCreated"]; got.Type != "string" || got.Format != "date-time" {
		t.Errorf("timeCreated = %+v, want string/date-time", got)
	}
	if got := types["metageneration"]; got.Type == "" {
		t.Errorf("metageneration has no type: %+v", got)
	}
}

// recorder answers every request with one item and records it by first path segment.
type recorder struct {
	mu   sync.Mutex
	reqs map[string]*http.Request
	form map[string]url.Values
}

func (rc *recorder) handler(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rc.mu.Lock()
	seg := strings.Split(strings.Trim(r.URL.Path, "/"), "/")[0]
	rc.reqs[seg], rc.form[r.URL.Path] = r, r.PostForm
	rc.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/token") {
		fmt.Fprintf(w, `{"access_token":"tok-%s","expires_in":3600}`, seg)
		return
	}
	fmt.Fprint(w, `{"items":[{"id":"1"}]}`)
}

func newRecorder() *recorder {
	return &recorder{reqs: map[string]*http.Request{}, form: map[string]url.Values{}}
}

func runNodes(t *testing.T, args omnisdk.Args, nodes ...omnisdk.Node) {
	t.Helper()
	g, err := omnisdk.NewGraph(nodes, nil)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(authRegistry, g, args)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if n == 0 {
		t.Fatal("no rows")
	}
}

// Two OAuth2 providers in one query, each exchanging its own id and secret at the token URL its
// document states — one with the account id filled from the environment — and each call carrying
// its own token.
func TestDocumentDeclaredClientCredentials(t *testing.T) {
	for k, v := range map[string]string{"TEST_OAUTHP_ID": "p-id", "TEST_OAUTHP_SECRET": "p-secret",
		"TEST_ACCOUNT_ID": "acc-1", "TEST_OAUTHQ_ID": "q-id", "TEST_OAUTHQ_SECRET": "q-secret"} {
		t.Setenv(k, v)
	}
	rc := newRecorder()
	srv := httptest.NewServer(http.HandlerFunc(rc.handler))
	defer srv.Close()
	runNodes(t, omnisdk.Args{Endpoint: srv.URL},
		omnisdk.NewNode("p", "stackql_unstable_oauthp.things.items", nil),
		omnisdk.NewNode("q", "stackql_unstable_oauthq.things.items", nil))

	pf := rc.form["/oidc/accounts/acc-1/v1/token"]
	if pf.Get("client_id") != "p-id" || pf.Get("client_secret") != "p-secret" || pf.Get("scope") != "all-apis" ||
		pf.Get("grant_type") != "client_credentials" {
		t.Errorf("oauthp token request = %v", pf)
	}
	if qf := rc.form["/q/token"]; qf.Get("client_id") != "q-id" || qf.Get("client_secret") != "q-secret" {
		t.Errorf("oauthq token request = %v, want its own credentials", qf)
	}
	if got := rc.reqs["oauthp"].Header.Get("Authorization"); got != "Bearer tok-oidc" {
		t.Errorf("oauthp call Authorization = %q", got)
	}
	if got := rc.reqs["oauthq"].Header.Get("Authorization"); got != "Bearer tok-q" {
		t.Errorf("oauthq call Authorization = %q", got)
	}
}

// A successor adds its header after its predecessor's.
func TestSuccessorAddsASecondHeader(t *testing.T) {
	t.Setenv("TEST_DD_API_KEY", "api-1")
	t.Setenv("TEST_DD_APP_KEY", "app-2")
	rc := newRecorder()
	srv := httptest.NewServer(http.HandlerFunc(rc.handler))
	defer srv.Close()
	runNodes(t, omnisdk.Args{Endpoint: srv.URL}, omnisdk.NewNode("d", "stackql_unstable_dualkey.things.items", nil))
	h := rc.reqs["dualkey"].Header
	if h.Get("DD-API-KEY") != "api-1" || h.Get("DD-APPLICATION-KEY") != "app-2" {
		t.Errorf("headers = %v, want both keys", h)
	}
}

// A kubeconfig context authenticates a cluster over its own TLS: the cluster's CA is trusted (no
// insecure skip), the user's credential is sent, and the server's {cluster_addr:pattern} variable
// binds like any other.
func TestKubeconfigContext(t *testing.T) {
	rc := newRecorder()
	srv := httptest.NewTLSServer(http.HandlerFunc(rc.handler))
	defer srv.Close()
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	host := strings.TrimPrefix(srv.URL, "https://")

	for name, user := range map[string]string{
		"token": "    token: kube-tok\n",
		"exec": "    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: /bin/sh\n" +
			"      args: [\"-c\", \"echo '{\\\"status\\\":{\\\"token\\\":\\\"kube-tok\\\"}}'\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: " + srv.URL +
				"\n    certificate-authority-data: " + ca + "\nusers:\n- name: u\n  user:\n" + user +
				"contexts:\n- name: ctx\n  context: {cluster: c, user: u}\n"
			runNodes(t, omnisdk.Args{
				Params:         map[string]string{"protocol": "https", "cluster_addr": host},
				AuthByProvider: map[string]*omnisdk.Auth{"kube": {Type: "kubeconfig", Credentials: kubeconfig, Context: "ctx"}},
			}, omnisdk.NewNode("k", "stackql_unstable_kube.things.items", nil))
			if got := rc.reqs["kube"].Header.Get("Authorization"); got != "Bearer kube-tok" {
				t.Errorf("Authorization = %q", got)
			}
		})
	}
}

// A kubeconfig without a context is refused: which cluster's credentials to send is not a default.
func TestKubeconfigNeedsAContext(t *testing.T) {
	g, _ := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("k", "stackql_unstable_kube.things.items", nil)}, nil)
	_, err := omnisdk.NewGraphSelectQuery(authRegistry, g, omnisdk.Args{
		Params:         map[string]string{"cluster_addr": "x"},
		AuthByProvider: map[string]*omnisdk.Auth{"kube": {Type: "kubeconfig", Credentials: "apiVersion: v1"}},
	})
	if err == nil || !strings.Contains(err.Error(), "needs a context") {
		t.Errorf("err = %v", err)
	}
}

// signingKey is the access key id in a SigV4 Authorization header.
func signingKey(authz string) string {
	_, cred, _ := strings.Cut(authz, "Credential=")
	k, _, _ := strings.Cut(cred, "/")
	return k
}

func iamUsersOnce(t *testing.T, args omnisdk.Args) string {
	t.Helper()
	requireCorpus(t)
	var authz string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<ListUsersResponse><ListUsersResult><Users><member><UserName>a</UserName></member></Users></ListUsersResult></ListUsersResponse>`)
	}))
	defer srv.Close()
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("u", iamUsers, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	args.Endpoint, args.Params = srv.URL, map[string]string{"region": "us-east-1"}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, g, args)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
	return signingKey(authz)
}

// With no keys in the environment, AWS signs with the shared-config profile: its static keys, or
// what its credential_process prints.
func TestAWSSharedConfigProfile(t *testing.T) {
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials")
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(creds, []byte("[static]\naws_access_key_id = AKIASTATIC\naws_secret_access_key = s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proc := `echo '{"Version":1,"AccessKeyId":"AKIAPROC","SecretAccessKey":"s","SessionToken":"t"}'`
	if err := os.WriteFile(cfg, []byte("[profile proc]\ncredential_process = "+proc+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	t.Setenv("AWS_CONFIG_FILE", cfg)

	t.Setenv("AWS_PROFILE", "static")
	if got := iamUsersOnce(t, omnisdk.Args{}); got != "AKIASTATIC" {
		t.Errorf("AWS_PROFILE=static signed with %q", got)
	}
	if got := iamUsersOnce(t, omnisdk.Args{AuthByProvider: map[string]*omnisdk.Auth{"aws": {Profile: "proc"}}}); got != "AKIAPROC" {
		t.Errorf("profile proc signed with %q", got)
	}
}

// A document reads only the credential it declares: a Google query never runs the AWS profile's
// credential_process, nor looks for Azure's service principal.
func TestOnlyTheDeclaredCredentialIsRead(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	cfg := filepath.Join(dir, "config")
	proc := `sh -c 'touch ` + marker + `; echo {"Version":1,"AccessKeyId":"A","SecretAccessKey":"s"}'`
	if err := os.WriteFile(cfg, []byte("[profile proc]\ncredential_process = "+proc+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_PROFILE", "proc")
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	srv, _, bearer := googleStub(t)
	defer srv.Close()
	bucketsOnce(t, srv, omnisdk.Args{})
	if *bearer != "Bearer g-tok" {
		t.Errorf("Authorization = %q", *bearer)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a Google query ran the AWS credential_process")
	}
}

// googleStub answers Google's token endpoint and a bucket list, recording the token request's form.
func googleStub(t *testing.T) (*httptest.Server, *url.Values, *string) {
	t.Helper()
	var form url.Values
	var bearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token") {
			form = r.PostForm
			fmt.Fprint(w, `{"access_token":"g-tok","expires_in":3600}`)
			return
		}
		bearer = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"items":[{"name":"b"}]}`)
	}))
	return srv, &form, &bearer
}

func bucketsOnce(t *testing.T, srv *httptest.Server, args omnisdk.Args) {
	t.Helper()
	requireCorpus(t)
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("b", "stackql_unstable_google.storage.buckets", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	args.Endpoint, args.Params = srv.URL, map[string]string{"project": "demo"}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, g, args)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
}

// gcloud's application-default user credential is exchanged by refresh token.
func TestGoogleUserCredential(t *testing.T) {
	t.Setenv("GOOGLE_CREDENTIALS", `{"type":"authorized_user","client_id":"cid","client_secret":"cs","refresh_token":"rt"}`)
	srv, form, bearer := googleStub(t)
	defer srv.Close()
	bucketsOnce(t, srv, omnisdk.Args{})
	if f := *form; f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != "rt" || f.Get("client_id") != "cid" {
		t.Errorf("token request = %v", f)
	}
	if *bearer != "Bearer g-tok" {
		t.Errorf("Authorization = %q", *bearer)
	}
}

// A service account acting as a user carries "sub", and asks for the scopes the caller names.
func TestGoogleDelegationSubjectAndScopes(t *testing.T) {
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	srv, form, _ := googleStub(t)
	defer srv.Close()
	scope := "https://www.googleapis.com/auth/admin.directory.user.readonly"
	bucketsOnce(t, srv, omnisdk.Args{AuthByProvider: map[string]*omnisdk.Auth{"google": {Subject: "admin@example.com", Scopes: []string{scope}}}})
	parts := strings.Split(form.Get("assertion"), ".")
	if len(parts) != 3 {
		t.Fatalf("assertion = %q", form.Get("assertion"))
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct{ Sub, Scope string }
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Sub != "admin@example.com" || claims.Scope != scope {
		t.Errorf("claims = %+v", claims)
	}
}

// With no scopes named by the caller or the document, a service account asks for cloud-platform,
// as any-sdk does.
func TestGoogleDefaultScopeIsCloudPlatform(t *testing.T) {
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	srv, form, _ := googleStub(t)
	defer srv.Close()
	bucketsOnce(t, srv, omnisdk.Args{})
	parts := strings.Split(form.Get("assertion"), ".")
	if len(parts) != 3 {
		t.Fatalf("assertion = %q", form.Get("assertion"))
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct{ Scope string }
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Scope != "https://www.googleapis.com/auth/cloud-platform" {
		t.Errorf("scope = %q", claims.Scope)
	}
}

// Without a service principal, an azure_default provider uses the Azure CLI's login.
func TestAzureCLICredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake az is a shell script")
	}
	requireCorpus(t)
	bin := t.TempDir()
	script := "#!/bin/sh\necho '{\"accessToken\":\"az-tok\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "az"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, v := range []string{"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET"} {
		t.Setenv(v, "")
	}
	var authz string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value":[{"name":"acct"}]}`)
	}))
	defer srv.Close()
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("a", "stackql_unstable_azure.storage.storage_accounts", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, g, omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"subscription_id": "sub"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
	if authz != "Bearer az-tok" {
		t.Errorf("Authorization = %q", authz)
	}
}

// On the path stackql takes — Resolve, then run — every node has a select list, so a row carries
// only table columns. Credentials are not columns: they stay out of the result under every
// redaction policy, not only the default.
func TestResolvedQueryNeverCarriesCredentials(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	srv, _, _ := googleStub(t)
	defer srv.Close()
	tbl, err := omnisdk.DescribeTable(corpus, "stackql_unstable_google.storage.buckets")
	if err != nil {
		t.Fatal(err)
	}
	q, err := query.New(
		[]query.Join{query.NewJoin(query.NewResource("b", "google.storage.buckets"), query.Base)},
		[]query.Predicate{query.NewEq(query.NewColumn("", "project"), query.NewLiteral("demo"))},
		[]query.Output{query.NewOutput("", query.NewStar(""))},
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"b": tbl})
	if err != nil {
		t.Fatal(err)
	}
	for name, policy := range map[string]omnisdk.Redaction{
		"unset": nil, "default": omnisdk.DefaultRedaction(), "none": omnisdk.RedactNone(),
	} {
		pl, err := omnisdk.NewGraphSelectQuery(corpus, res.Graph(),
			omnisdk.Args{Endpoint: srv.URL, Params: res.Params(), Redaction: policy})
		if err != nil {
			t.Fatalf("%s: plan: %v", name, err)
		}
		rows, err := pl.Open(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		n := 0
		for rows.Next() {
			n++
			for _, k := range []string{"token", "assertion", "access_token"} {
				if _, leaked := rows.Row()[k]; leaked {
					t.Errorf("%s: row carries %q", name, k)
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rows.Close()
		if n != 1 {
			t.Errorf("%s: %d rows, want 1", name, n)
		}
	}
}

// One query reads AWS, Google and Azure, and every request carries its own provider's credential:
// AWS signed with AWS keys, Google with the token its service account bought, Azure with the token
// its service principal bought. None carries another's.
func TestOneQueryAcrossThreeClouds(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	t.Setenv("AZURE_TENANT_ID", "tenant")
	t.Setenv("AZURE_CLIENT_ID", "client")
	t.Setenv("AZURE_CLIENT_SECRET", "az-secret")
	var mu sync.Mutex
	authz := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/oauth2/"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"az-tok","expires_in":3600}`)
		case strings.HasSuffix(r.URL.Path, "/token"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"g-tok","expires_in":3600}`)
		case r.Form.Get("Action") == "ListUsers" || r.URL.Query().Get("Action") == "ListUsers":
			authz["aws"] = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, `<ListUsersResponse><ListUsersResult><Users>`+
				`<member><UserName>alice</UserName></member><member><UserName>bob</UserName></member>`+
				`</Users></ListUsersResult></ListUsersResponse>`)
		case strings.Contains(r.URL.Path, "virtualNetworks"):
			authz["azure"] = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"value":[{"name":"vnet-1"}]}`)
		case strings.Contains(r.URL.Path, "/b"):
			authz["google"] = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"items":[{"name":"bucket-1"}]}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusNotFound)
		}
	}))
	defer srv.Close()

	g, err := omnisdk.NewGraph([]omnisdk.Node{
		omnisdk.NewNode("u", "stackql_unstable_aws.iam.users", nil),
		omnisdk.NewNode("b", "stackql_unstable_google.storage.buckets", nil),
		omnisdk.NewNode("v", "stackql_unstable_azure.network.virtual_networks", nil),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, g, omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{
		"region": "us-east-1", "project": "demo", "subscription_id": "sub"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
	if n != 2 {
		t.Errorf("rows = %d, want 2 users × 1 bucket × 1 vnet", n)
	}
	if got := authz["aws"]; !strings.HasPrefix(got, "AWS4-HMAC-SHA256 Credential=AKIATEST/") {
		t.Errorf("aws: Authorization = %q", got)
	}
	if got := authz["google"]; got != "Bearer g-tok" {
		t.Errorf("google: Authorization = %q", got)
	}
	if got := authz["azure"]; got != "Bearer az-tok" {
		t.Errorf("azure: Authorization = %q", got)
	}
}
