package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
	"github.com/stackql-labs/omnisdk/pkg/rowcache"
)

const cachedAddress = "stackql_unstable_cached.s.items"

func cachedFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"provider.yaml": "name: cached\nproviderServices:\n  s:\n    name: s\n    service: {$ref: 'services/s.yaml'}\n",
		"services/s.yaml": `openapi: 3.0.0
servers: [{url: 'https://example.test'}]
paths:
  /items:
    get:
      parameters: [{name: region, in: query, schema: {type: string}}]
      responses:
        '200':
          content:
            application/json:
              schema:
                type: object
                properties:
                  items:
                    type: array
                    items:
                      type: object
                      properties: {id: {type: integer}, name: {type: string}}
    post:
      responses:
        '200':
          content:
            application/json:
              schema: {type: object}
components:
  x-stackQL-resources:
    items:
      methods:
        list:
          operation: {$ref: '#/paths/~1items/get'}
          response: {mediaType: application/json, objectKey: '$.items'}
        create:
          operation: {$ref: '#/paths/~1items/post'}
          response: {mediaType: application/json}
      sqlVerbs:
        select: [{$ref: '#/components/x-stackQL-resources/items/methods/list'}]
        insert: [{$ref: '#/components/x-stackQL-resources/items/methods/create'}]
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
func cachedArgs(t *testing.T, endpoint string) omnisdk.Args {
	t.Helper()
	c, err := rowcache.NewMemory(context.Background(), rowcache.Config{}, rowcache.MemoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return omnisdk.Args{Endpoint: endpoint, ReadCache: c, ReadCachePartition: "tenant-a"}
}
func cachedPlan(t *testing.T, dir string, args omnisdk.Args, field string, id int) omnisdk.Plan {
	t.Helper()
	table, err := omnisdk.DescribeTable(dir, cachedAddress)
	if err != nil {
		t.Fatal(err)
	}
	var predicates []query.Predicate
	if id != 0 {
		predicates = append(predicates, query.NewEq(query.NewColumn("i", "id"), query.NewLiteral(id)))
	}
	q, err := query.New([]query.Join{query.NewJoin(query.NewResource("i", cachedAddress), query.Base)}, predicates,
		[]query.Output{query.NewOutput(field, query.NewColumn("i", field))})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"i": table})
	if err != nil {
		t.Fatal(err)
	}
	p, err := omnisdk.NewGraphQuery(dir, resolved.Graph(), args)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func cacheRows(t *testing.T, p omnisdk.Plan) []omnisdk.Row {
	t.Helper()
	rows, err := p.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []omnisdk.Row
	for rows.Next() {
		out = append(out, rows.Row())
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestReadCacheReusesRawRowsAcrossProjectionAndFilter(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"items":[{"id":1,"name":"one"},{"id":2,"name":"two"}]}`)
	}))
	defer server.Close()
	dir := cachedFixture(t)
	args := cachedArgs(t, server.URL)
	first := cacheRows(t, cachedPlan(t, dir, args, "id", 1))
	if len(first) != 1 {
		t.Fatal(first)
	}
	second := cacheRows(t, cachedPlan(t, dir, args, "name", 2))
	if !reflect.DeepEqual(second, []omnisdk.Row{{"name": "two"}}) {
		t.Fatal(second)
	}
	if calls.Load() != 1 {
		t.Fatalf("requests=%d; cache must precede query filters/projection", calls.Load())
	}
	other := args
	other.ReadCachePartition = "tenant-b"
	cacheRows(t, cachedPlan(t, dir, other, "id", 0))
	if calls.Load() != 2 {
		t.Fatal("authorization partitions shared rows")
	}
	other = args
	other.Params = map[string]string{"region": "east"}
	cacheRows(t, cachedPlan(t, dir, other, "id", 0))
	if calls.Load() != 3 {
		t.Fatal("request inputs shared rows")
	}
}
func TestReadCacheDoesNotPublishCappedReads(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"items":[{"id":1,"name":"one"},{"id":2,"name":"two"}]}`)
	}))
	defer server.Close()
	dir := cachedFixture(t)
	args := cachedArgs(t, server.URL)
	for _, tuning := range []omnisdk.Tuning{{PageLimit: 1}, {Limit: 1}} {
		capped := args
		capped.ReadCachePartition = fmt.Sprint(tuning)
		capped.Tuning = tuning
		cacheRows(t, cachedPlan(t, dir, capped, "id", 0))
		capped.Tuning = omnisdk.Tuning{}
		if got := cacheRows(t, cachedPlan(t, dir, capped, "id", 0)); len(got) != 2 {
			t.Fatal(got)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("capped reads reused or published entries: requests=%d", calls.Load())
	}
}

type ignoreNotFound struct{}

func (ignoreNotFound) IsIgnored(status int) bool { return status == 404 }
func TestReadCacheDoesNotPublishIgnoredHTTPFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(404)
			return
		}
		fmt.Fprint(w, `{"items":[{"id":1,"name":"one"}]}`)
	}))
	defer server.Close()
	args := cachedArgs(t, server.URL)
	args.HTTPReadErrors = ignoreNotFound{}
	p := cachedPlan(t, cachedFixture(t), args, "name", 0)
	if got := cacheRows(t, p); len(got) != 0 {
		t.Fatal(got)
	}
	if got := cacheRows(t, p); len(got) != 1 {
		t.Fatal("ignored error was cached", got)
	}
	cacheRows(t, p)
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
}
func TestReadCacheNeverCachesMutations(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{"id":1}`) }))
	defer server.Close()
	args := cachedArgs(t, server.URL)
	graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("i", cachedAddress, "insert", nil, nil, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := omnisdk.NewGraphQuery(cachedFixture(t), graph, args)
	if err != nil {
		t.Fatal(err)
	}
	cacheRows(t, p)
	cacheRows(t, p)
	if calls.Load() != 2 {
		t.Fatal("mutation response reused")
	}
}
func TestReadCacheRequiresPartitionAndTracksDocumentChanges(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"items":[{"id":1,"name":"one"}]}`)
	}))
	defer server.Close()
	dir := cachedFixture(t)
	args := cachedArgs(t, server.URL)
	p := cachedPlan(t, dir, args, "id", 0)
	cacheRows(t, p)
	file := filepath.Join(dir, "services/s.yaml")
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("\n# revised provider contract\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cacheRows(t, cachedPlan(t, dir, args, "id", 0))
	if calls.Load() != 2 {
		t.Fatal("document revision reused old rows")
	}
	args.ReadCachePartition = ""
	if _, err = omnisdk.NewSelectFromCatalog(dir, cachedAddress, args); err == nil {
		t.Fatal("missing partition accepted")
	}
}

func TestReadCachePaginationAndLateFailure(t *testing.T) {
	var calls atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("pageToken") == "two" {
			if failing.Load() {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"second page failed"}`)
				return
			}
			fmt.Fprint(w, `{"items":[{"id":2,"name":"two"}]}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"id":1,"name":"one"}],"nextPageToken":"two"}`)
	}))
	defer server.Close()
	args := cachedArgs(t, server.URL)
	p := cachedPlan(t, cachedFixture(t), args, "id", 0)
	rows, err := p.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if rows.Err() == nil {
		t.Fatal("late HTTP failure hidden")
	}
	rows.Close()
	afterFailure := calls.Load()
	failing.Store(false)
	if got := cacheRows(t, p); len(got) != 2 {
		t.Fatal("partial entry was published", got)
	}
	afterSuccess := calls.Load()
	if afterSuccess-afterFailure != 2 {
		t.Fatal("missing pagination", calls.Load())
	}
	if got := cacheRows(t, p); len(got) != 2 {
		t.Fatal(got)
	}
	if calls.Load() != afterSuccess {
		t.Fatal("complete paginated read not cached")
	}
}
func TestReadCacheHonorsProviderPageCap(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"items":[{"id":1,"name":"one"}]}`)
	}))
	defer server.Close()
	args := cachedArgs(t, server.URL)
	args.TuningByProvider = map[string]omnisdk.ProviderTuning{"cached": {PageLimit: 1}}
	dir := cachedFixture(t)
	cacheRows(t, cachedPlan(t, dir, args, "id", 0))
	cacheRows(t, cachedPlan(t, dir, args, "id", 0))
	if calls.Load() != 2 {
		t.Fatal("provider-capped read reused an entry")
	}
	args.TuningByProvider = nil
	cacheRows(t, cachedPlan(t, dir, args, "id", 0))
	cacheRows(t, cachedPlan(t, dir, args, "id", 0))
	if calls.Load() != 3 {
		t.Fatal("provider-capped read published an entry")
	}
}

func TestReadCacheDoesNotPublishToleratedSelectionFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, `{}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"id":1,"name":"one"}]}`)
	}))
	defer server.Close()
	dir := cachedFixture(t)
	args := cachedArgs(t, server.URL)
	args.StrictResponseSelection = true
	args.ResponseSelectionErrors = &missingKeyPolicy{}
	plan := cachedPlan(t, dir, args, "id", 0)
	if rows := cacheRows(t, plan); len(rows) != 0 {
		t.Fatal(rows)
	}
	if rows := cacheRows(t, plan); len(rows) != 1 {
		t.Fatal(rows)
	}
	if rows := cacheRows(t, plan); len(rows) != 1 {
		t.Fatal(rows)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests=%d; failed selection must not become cached absence", calls.Load())
	}
}

func TestSnapshotCacheLookupsKeepLiveMissesUncached(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"items":[{"id":1,"name":"one"}]}`)
	}))
	defer server.Close()
	dir := cachedFixture(t)
	args := cachedArgs(t, server.URL)
	cacheRows(t, cachedPlan(t, dir, args, "id", 0))
	args.Tuning.PageLimit = 1
	args.ReadCacheLookupOnly = true
	cacheRows(t, cachedPlan(t, dir, args, "id", 0))
	if calls.Load() != 1 {
		t.Fatal("completed snapshot was not reused")
	}
	args.ReadCachePartition = "new-scope"
	cacheRows(t, cachedPlan(t, dir, args, "id", 0))
	cacheRows(t, cachedPlan(t, dir, args, "id", 0))
	if calls.Load() != 3 {
		t.Fatal("snapshot-only misses published a partial live read")
	}
}

func TestReadErrorPolicyNeverSwallowsDocumentMutationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	args := omnisdk.Args{Endpoint: server.URL, HTTPReadErrors: ignoreNotFound{}, Tuning: omnisdk.Tuning{RetryTries: 1}}
	graph, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("i", cachedAddress, "insert", nil, nil, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := omnisdk.NewGraphQuery(cachedFixture(t), graph, args)
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
	if rows.Err() == nil {
		t.Fatal("read error policy silently accepted a failed mutation")
	}
}
