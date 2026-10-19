package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

// A query on a resource whose document declares OData pushdown asks the server for what it reads: the
// select list and the filter's columns as $select, the filter as $filter, and — where nothing after
// the server could drop a row — the limit as $top. The filter still runs on the rows, so the server
// answering more than it was asked for does not change the result.
func TestSelectAndWherePushDown(t *testing.T) {
	asked, got := runUsersQuery(t, []query.Predicate{
		query.NewEq(query.NewColumn("u", "accountEnabled"), query.NewLiteral(true)),
		query.NewEq(query.NewColumn("u", "displayName"), query.NewLiteral("Ann")),
	})
	if strings.Join(got, ",") != "Ann" {
		t.Errorf("rows = %v, want only Ann: the filter still runs on what comes back", got)
	}
	if s := asked.Get("$select"); s != "accountEnabled,displayName" {
		t.Errorf("$select = %q, want the select list and the filter's columns", s)
	}
	if f := asked.Get("$filter"); f != "accountEnabled eq true and displayName eq 'Ann'" {
		t.Errorf("$filter = %q", f)
	}
	if top := asked.Get("$top"); top != "5" {
		t.Errorf("$top = %q, want the limit", top)
	}
}

// A filter the server cannot take stays on the rows, and then the limit cannot go to the server: a
// row the server counts towards it might be one the filter drops.
func TestUnpushableFilterKeepsTheLimitClientSide(t *testing.T) {
	asked, got := runUsersQuery(t, []query.Predicate{
		query.NewOr(query.NewEq(query.NewColumn("u", "displayName"), query.NewLiteral("Ann")),
			query.NewEq(query.NewColumn("u", "displayName"), query.NewLiteral("Zed"))),
	})
	if strings.Join(got, ",") != "Ann" {
		t.Errorf("rows = %v, want only Ann", got)
	}
	if asked.Has("$filter") || asked.Has("$top") {
		t.Errorf("query = %v, want no $filter and no $top", asked)
	}
	if s := asked.Get("$select"); s != "displayName" {
		t.Errorf("$select = %q", s)
	}
}

// runUsersQuery runs SELECT displayName AS name FROM entra users WHERE where, LIMIT 5, against a
// stand-in answering Ann and Bob, and reports what the stand-in was asked and the names returned.
func runUsersQuery(t *testing.T, where []query.Predicate) (url.Values, []string) {
	t.Helper()
	requireCorpus(t)
	t.Setenv("AZURE_TENANT_ID", "t")
	t.Setenv("AZURE_CLIENT_ID", "c")
	t.Setenv("AZURE_CLIENT_SECRET", "s")
	var mu sync.Mutex
	var asked url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"tok"}`)
			return
		}
		mu.Lock()
		asked = r.URL.Query()
		mu.Unlock()
		// One row the filter rejects: a server that ignored part of $filter must not leak it.
		fmt.Fprint(w, `{"value":[{"displayName":"Ann","accountEnabled":true,"id":"1"},{"displayName":"Bob","accountEnabled":true,"id":"2"}]}`)
	}))
	defer srv.Close()

	const users = "stackql_unstable_entra_id.users.users"
	tbl, err := omnisdk.DescribeTable(corpus, users)
	if err != nil {
		t.Fatal(err)
	}
	q, err := query.New(
		[]query.Join{query.NewJoin(query.NewResource("u", "entra_id.users.users"), query.Base)},
		where,
		[]query.Output{query.NewOutput("name", query.NewColumn("u", "displayName"))},
	)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.Prepare(corpus, omnisdk.SQLStatement(q, map[string]omnisdk.Table{"u": tbl}), omnisdk.NoWAL(),
		omnisdk.Args{Endpoint: srv.URL, Tuning: omnisdk.Tuning{Limit: 5}})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		got = append(got, fmt.Sprint(rows.Row()["name"]))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return asked, got
}
