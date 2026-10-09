package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

// countRows plans one node against dir and counts what it returns.
func countRows(t *testing.T, dir, address string, args omnisdk.Args) int {
	t.Helper()
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("n", address, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(dir, g, args)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return n
}

// A document declaring Link-header pagination follows rel="next" until there is none.
func TestPaginationFollowsTheLinkHeader(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+srv.URL+`/linked/items?page=2>; rel="next", <`+srv.URL+`/linked/items?page=2>; rel="last"`)
			fmt.Fprint(w, `[{"id":"1"},{"id":"2"}]`)
			return
		}
		fmt.Fprint(w, `[{"id":"3"}]`)
	}))
	defer srv.Close()
	if n := countRows(t, authRegistry, "stackql_unstable_linked.things.items", omnisdk.Args{Endpoint: srv.URL}); n != 3 {
		t.Errorf("rows = %d, want both pages", n)
	}
}

// Google declares no pagination, but a list taking pageToken whose response has nextPageToken
// states it: every page is read.
func TestPaginationByPageToken(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			fmt.Fprint(w, `{"access_token":"tok"}`)
		case r.URL.Query().Get("pageToken") == "":
			fmt.Fprint(w, `{"items":[{"name":"a"}],"nextPageToken":"p2"}`)
		case r.URL.Query().Get("pageToken") == "p2":
			fmt.Fprint(w, `{"items":[{"name":"b"}]}`)
		default:
			http.Error(w, "unexpected token", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	n := countRows(t, corpus, "stackql_unstable_google.storage.buckets",
		omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"project": "demo"}})
	if n != 2 {
		t.Errorf("rows = %d, want both pages", n)
	}
}

// Microsoft Graph's x-ms-pageable next link is followed.
func TestPaginationFollowsODataNextLink(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AZURE_TENANT_ID", "t")
	t.Setenv("AZURE_CLIENT_ID", "c")
	t.Setenv("AZURE_CLIENT_SECRET", "s")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			fmt.Fprint(w, `{"access_token":"tok"}`)
		case r.URL.Query().Get("skiptoken") == "":
			fmt.Fprintf(w, `{"value":[{"displayName":"a"}],"@odata.nextLink":"%s/v1.0/directoryRoles?skiptoken=2"}`, srv.URL)
		default:
			fmt.Fprint(w, `{"value":[{"displayName":"b"}]}`)
		}
	}))
	defer srv.Close()
	if n := countRows(t, corpus, "stackql_unstable_entra_id.directory_roles.directory_roles", omnisdk.Args{Endpoint: srv.URL}); n != 2 {
		t.Errorf("rows = %d, want both pages", n)
	}
}

// Rows stream: the first page's rows reach the caller while the second page has not been answered.
func TestRowsArriveBeforeTheLastPage(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			fmt.Fprint(w, `{"access_token":"tok"}`)
		case r.URL.Query().Get("pageToken") == "":
			fmt.Fprint(w, `{"items":[{"name":"a"}],"nextPageToken":"p2"}`)
		default:
			<-release
			fmt.Fprint(w, `{"items":[{"name":"b"}]}`)
		}
	}))
	defer srv.Close()
	defer close(release)
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("b", "stackql_unstable_google.storage.buckets", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, g, omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"project": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make(chan bool, 1)
	go func() { got <- rows.Next() }()
	select {
	case ok := <-got:
		if !ok {
			t.Fatalf("no first row: %v", rows.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first row waited for the last page")
	}
}

// IAM declares no pagination, but its Marker parameter pages: a truncated reply carries the next
// Marker, and every page is read.
func TestPaginationByAWSMarker(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "text/xml")
		if r.Form.Get("Marker") == "" {
			fmt.Fprint(w, `<ListUsersResponse><ListUsersResult><Users><member><UserName>a</UserName></member></Users>`+
				`<IsTruncated>true</IsTruncated><Marker>m2</Marker></ListUsersResult></ListUsersResponse>`)
			return
		}
		fmt.Fprint(w, `<ListUsersResponse><ListUsersResult><Users><member><UserName>b</UserName></member></Users>`+
			`<IsTruncated>false</IsTruncated></ListUsersResult></ListUsersResponse>`)
	}))
	defer srv.Close()
	n := countRows(t, corpus, iamUsers, omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"region": "us-east-1"}})
	if n != 2 {
		t.Errorf("rows = %d, want both pages", n)
	}
}

// Memory is bounded by a page, not by the result: a query resolved as stackql resolves it streams
// several times the bound through the caller while the heap stays under it.
//
// It measures in a fresh process: in a shared one, what earlier tests left behind is collected
// during the stream and swamps what the stream itself holds.
func TestResolvedQueryStreamsInBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("streams ~50MB")
	}
	requireCorpus(t)
	if os.Getenv("OMNISDK_MEMTEST") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestResolvedQueryStreamsInBoundedMemory$", "-test.v")
		cmd.Env = append(os.Environ(), "OMNISDK_MEMTEST=1")
		out, err := cmd.CombinedOutput()
		t.Logf("%s", out)
		if err != nil {
			t.Fatalf("measuring process: %v", err)
		}
		return
	}
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	const pages, perPage, bound = 100, 500, 16 << 20
	pad := strings.Repeat("x", 1024)
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"tok"}`)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
		var b strings.Builder
		b.WriteString(`{"items":[`)
		for i := 0; i < perPage; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"name":"b-%d-%d","location":%q}`, page, i, pad)
		}
		b.WriteString(`]`)
		if page+1 < pages {
			fmt.Fprintf(&b, `,"nextPageToken":"%d"`, page+1)
		}
		b.WriteString(`}`)
		served.Add(int64(b.Len()))
		fmt.Fprint(w, b.String())
	}))
	defer srv.Close()

	tbl, err := omnisdk.DescribeTable(corpus, "stackql_unstable_google.storage.buckets")
	if err != nil {
		t.Fatal(err)
	}
	q, err := query.New(
		[]query.Join{query.NewJoin(query.NewResource("b", "google.storage.buckets"), query.Base)},
		[]query.Predicate{query.NewEq(query.NewColumn("", "project"), query.NewLiteral("demo"))},
		[]query.Output{
			query.NewOutput("name", query.NewColumn("b", "name")),
			query.NewOutput("location", query.NewColumn("b", "location")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	res, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"b": tbl})
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, res.Graph(), omnisdk.Args{Endpoint: srv.URL, Params: res.Params()})
	if err != nil {
		t.Fatal(err)
	}

	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	base := heap()
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	var peak uint64
	for rows.Next() {
		n++
		if n%perPage == 0 {
			if h := heap(); h > peak {
				peak = h
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != pages*perPage {
		t.Fatalf("rows = %d, want %d", n, pages*perPage)
	}
	if served.Load() < 3*bound {
		t.Fatalf("served %d bytes; too little to prove a %d-byte bound", served.Load(), bound)
	}
	grew := int64(peak) - int64(base)
	t.Logf("served %d MB, heap grew at most %d MB", served.Load()>>20, grew>>20)
	if grew > bound {
		t.Errorf("heap grew %d MB streaming %d MB: rows are being held", grew>>20, served.Load()>>20)
	}
}

// Read-ahead is the caller's: bounded, a reader that stops after one row stops the paging a few
// pages on; unbounded, every page is fetched whether or not anyone reads it.
func TestReadAheadIsTunable(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	const pages = 200
	fetched := func(tuning omnisdk.Tuning) int64 {
		var served atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/token") {
				fmt.Fprint(w, `{"access_token":"tok"}`)
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
			served.Add(1)
			if page+1 < pages {
				fmt.Fprintf(w, `{"items":[{"name":"b-%d"}],"nextPageToken":"%d"}`, page, page+1)
				return
			}
			fmt.Fprintf(w, `{"items":[{"name":"b-%d"}]}`, page)
		}))
		defer srv.Close()
		g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("b", "stackql_unstable_google.storage.buckets", nil)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		pl, err := omnisdk.NewGraphSelectQuery(corpus, g, omnisdk.Args{Endpoint: srv.URL,
			Params: map[string]string{"project": "demo"}, Tuning: tuning})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := pl.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("no first row: %v", rows.Err())
		}
		// Let the producers run as far ahead as they are allowed.
		deadline := time.Now().Add(3 * time.Second)
		for last := int64(-1); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			n := served.Load()
			if n == last || n == pages {
				break
			}
			last = n
		}
		return served.Load()
	}
	if n := fetched(omnisdk.Tuning{RowsAhead: 1, PagesAhead: 1}); n > pages/4 {
		t.Errorf("bounded: fetched %d of %d pages for one row read", n, pages)
	}
	if n := fetched(omnisdk.Tuning{RowsAhead: -1, PagesAhead: -1}); n != pages {
		t.Errorf("unbounded: fetched %d of %d pages", n, pages)
	}
}

// A cursor dropped without Close does not leave its run parked: once the cursor is collected, the run
// is cancelled, and the page request it had in flight is abandoned.
func TestDroppedRowsCancelTheRun(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	cancelled := make(chan struct{}, 1)
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			fmt.Fprint(w, `{"access_token":"tok"}`)
		case r.URL.Query().Get("pageToken") == "":
			fmt.Fprint(w, `{"items":[{"name":"a"}],"nextPageToken":"p2"}`)
		default:
			// The second page never answers; only the run being cancelled ends this request.
			select {
			case <-r.Context().Done():
				cancelled <- struct{}{}
			case <-stop:
			}
		}
	}))
	defer srv.Close()
	defer close(stop)

	readOneAndDrop(t, srv.URL)
	deadline := time.After(10 * time.Second)
	for {
		runtime.GC()
		select {
		case <-cancelled:
			return
		case <-deadline:
			t.Fatal("the run outlived its dropped cursor")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// readOneAndDrop opens a run, reads one row and returns without closing it, so nothing refers to the
// cursor afterwards.
func readOneAndDrop(t *testing.T, endpoint string) {
	t.Helper()
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("b", "stackql_unstable_google.storage.buckets", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, g, omnisdk.Args{Endpoint: endpoint, Params: map[string]string{"project": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("no first row: %v", rows.Err())
	}
}

// A GitHub document that declares no pagination still follows the Link header, as any-sdk does by
// default for GitHub.
func TestGithubDefaultsToLinkPagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+srv.URL+`/orgs/o/members?page=2>; rel="next"`)
			fmt.Fprint(w, `[{"login":"a"}]`)
			return
		}
		fmt.Fprint(w, `[{"login":"b"}]`)
	}))
	defer srv.Close()
	if n := countRows(t, authRegistry, "stackql_unstable_github.orgs.members",
		omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"org": "o"}}); n != 2 {
		t.Errorf("rows = %d, want both pages", n)
	}
}
