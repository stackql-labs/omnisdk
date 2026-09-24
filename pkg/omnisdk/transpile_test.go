package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

const (
	iamUsers            = "stackql_unstable_aws.iam.users"
	iamAttachedPolicies = "stackql_unstable_aws.iam.attached_user_policies"
)

/*
This file is a worked specification of what a SQL front end (stackql) must produce to run a
"simple" query — joins, functions and projections only — through the facade. It asserts the
MAPPING, not the network: whether the calls go out correctly is covered by projection_test.go.
*/

/*
The query:

	SELECT u.UserName, p.PolicyName
	FROM aws.iam.users u INNER JOIN aws.iam.attached_user_policies p ON p.UserName = u.UserName
	where region = 'us-east-1'
	;

The join is a REAL dependency, which is why it is the example: list_attached_user_policies
requires UserName, so the child cannot be listed at all without the parent. A pair that both
list flat (Google networks and subnetworks) would make the edge look optional, which is the
opposite of what a transpiler has to reason about.

Every clause lands in exactly one place, and the whole of it is three arguments to
NewGraphSelectQuery: a directory, a Graph and Args. The graph is keyed by ALIAS, as the query is:
u and p are the identities, the addresses are what they run.
*/
func TestTranspileSimpleJoin(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	// FROM and JOIN name the relations. Each table reference is one node: its alias and the address
	// it runs.
	nodes := []omnisdk.Node{
		omnisdk.NewNode("u", iamUsers, nil),
		omnisdk.NewNode("p", iamAttachedPolicies, nil),
	}

	// ON p.UserName = u.UserName becomes a β edge. It is DIRECTED, and here the direction is forced:
	// only users can be listed without the other, so users produces and the policies consume. `via`
	// turns the joined value into the parameter the API takes, and `provides` names it.
	join := omnisdk.NewWiring(
		"p",
		[]omnisdk.Inbound{omnisdk.NewInbound("u", "UserName", "user_name")},
		"golang_template_json_v0.1.0",
		`{"UserName":"{{ .user_name }}"}`,
		"UserName",
	)

	// The SELECT list becomes one projection per node. A computed column is a function call; a
	// column the join needs is retained even where the user never asked for it, because the
	// projection REPLACES the row and the edge reads it afterwards.
	usersSelect, err := omnisdk.NewProjection("u", []omnisdk.SelectColumn{
		omnisdk.NewSelectColumn("UserName", omnisdk.NewField("UserName")),
	})
	if err != nil {
		t.Fatalf("users projection: %v", err)
	}
	policiesSelect, err := omnisdk.NewProjection("p", []omnisdk.SelectColumn{
		omnisdk.NewSelectColumn("PolicyName", omnisdk.NewField("PolicyName")),
	})
	if err != nil {
		t.Fatalf("policies projection: %v", err)
	}

	g, err := omnisdk.NewGraphWithProjections(nodes, []omnisdk.Wiring{join},
		[]omnisdk.Projection{usersSelect, policiesSelect})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}

	// The WHERE conjuncts the targets accept as request inputs are pushdown, and pushdown is exactly
	// Args.Params. This query has none beyond the scope the signing needs.
	args := omnisdk.Args{Params: map[string]string{"region": "us-east-1"}}

	// The mapping itself, clause by clause, asserted against the graph a transpiler would emit.
	var got []string
	for _, n := range g.Nodes() {
		got = append(got, n.Alias()+"="+n.Address())
	}
	if want := []string{"u=" + iamUsers, "p=" + iamAttachedPolicies}; !reflect.DeepEqual(got, want) {
		t.Errorf("FROM/JOIN -> nodes = %v, want %v", got, want)
	}

	ws := g.Wirings()
	if len(ws) != 1 {
		t.Fatalf("ON -> wirings = %d, want 1", len(ws))
	}
	w := ws[0]
	if w.To() != "p" {
		t.Errorf("the consumer is the side that cannot list alone; got %s", w.To())
	}
	in := w.Inbound()
	if len(in) != 1 || in[0].From() != "u" || in[0].Src() != "UserName" {
		t.Errorf("ON -> inbound = %#v", in)
	}
	if !reflect.DeepEqual(w.Provides(), []string{"UserName"}) {
		t.Errorf("via builds the consumer's parameter; provides = %v", w.Provides())
	}

	// One projection per node, carrying that relation's share of the select list.
	wantCols := map[string][]string{
		"u": {"UserName"},
		"p": {"PolicyName"},
	}
	for _, p := range g.Projections() {
		var got []string
		for _, c := range p.Columns() {
			got = append(got, c.Out())
		}
		if want := wantCols[p.Alias()]; !reflect.DeepEqual(got, want) {
			t.Errorf("SELECT -> %s columns = %v, want %v", p.Alias(), got, want)
		}
	}

	// Composition is the last assertion: an exchange whose required input nothing supplies, or an
	// edge onto a node the graph does not run, fails HERE — before a request is made.
	if _, err := omnisdk.NewGraphSelectQuery(corpus, g, args); err != nil {
		t.Fatalf("plan: %v", err)
	}
}

// iamStub answers ListUsers with alice and bob, and ListAttachedUserPolicies with one policy named
// after the user asked about — so the join is visible in the rows.
func iamStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "text/xml")
		switch r.Form.Get("Action") {
		case "ListUsers":
			fmt.Fprint(w, `<ListUsersResponse><ListUsersResult><Users>`+
				`<member><UserName>alice</UserName></member><member><UserName>bob</UserName></member>`+
				`</Users></ListUsersResult></ListUsersResponse>`)
		case "GetUser":
			fmt.Fprintf(w, `<GetUserResponse><GetUserResult><User><UserName>%s</UserName></User></GetUserResult></GetUserResponse>`,
				r.Form.Get("UserName"))
		case "ListAttachedUserPolicies":
			fmt.Fprintf(w, `<ListAttachedUserPoliciesResponse><ListAttachedUserPoliciesResult><AttachedPolicies>`+
				`<member><PolicyName>%s-policy</PolicyName></member>`+
				`</AttachedPolicies></ListAttachedUserPoliciesResult></ListAttachedUserPoliciesResponse>`, r.Form.Get("UserName"))
		default:
			http.Error(w, "unexpected action "+r.Form.Get("Action"), http.StatusBadRequest)
		}
	}))
}

/*
The real-world query, end to end:

		SELECT u.UserName, p.PolicyName
		FROM aws.iam.users u INNER JOIN aws.iam.attached_user_policies p ON p.UserName = u.UserName
		where region = 'us-east-1'
		;

	 1. The front end states it as a query.Unresolved: resources, joins, bindings and projection exactly
	    as written. Nothing a document would have to say.
	 2. Each resource is described from the documents: its select methods, their parameters and
	    columns. The handle aws.iam.users names a provider this registry publishes as
	    stackql_unstable_aws; that naming is the front end's, so it is stated here, not inferred.
	 3. Resolve places each binding — the ON becomes an edge, directed by which side's methods require
	    UserName; region becomes a query-wide param — and the graph runs.
*/
func TestRealWorldQueryEndToEnd(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	srv := iamStub(t)
	defer srv.Close()

	// 1. The query, as written.
	q, err := query.New(
		[]query.Join{
			query.NewJoin(query.NewResource("u", "aws.iam.users"), query.Base),
			query.NewJoin(query.NewResource("p", "aws.iam.attached_user_policies"), query.Inner,
				query.NewEq(query.NewColumn("p", "UserName"), query.NewColumn("u", "UserName"))),
		},
		[]query.Predicate{
			query.NewEq(query.NewColumn("", "region"), query.NewLiteral("us-east-1")),
		},
		[]query.Output{
			query.NewOutput("UserName", query.NewColumn("u", "UserName")),
			query.NewOutput("PolicyName", query.NewColumn("p", "PolicyName")),
		},
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	// 2. What the documents say about each resource.
	providers := map[string]string{"aws": "stackql_unstable_aws"}
	tables := map[string]omnisdk.Table{}
	for _, j := range q.From() {
		r := j.Resource()
		provider, rest, _ := strings.Cut(r.Handle(), ".")
		tbl, err := omnisdk.DescribeTable(corpus, providers[provider]+"."+rest)
		if err != nil {
			t.Fatalf("describe %s: %v", r.Alias(), err)
		}
		tables[r.Alias()] = tbl
	}

	// 3. Resolve and run.
	res, err := omnisdk.Resolve(q, tables)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, res.Graph(), omnisdk.Args{Endpoint: srv.URL, Params: res.Params()})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		r := rows.Row()
		got = append(got, fmt.Sprintf("%v/%v", r["UserName"], r["PolicyName"]))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(got)
	if want := []string{"alice/alice-policy", "bob/bob-policy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// runQuery resolves q against the corpus and returns its rows as "col=value" strings, sorted, and
// the IAM calls made as "action|signing region".
func runQuery(t *testing.T, q query.Unresolved) ([]string, []string) {
	t.Helper()
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	var seen calls
	stub := iamStub(t)
	defer stub.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seen.add(r.Form.Get("Action") + "|" + signingRegion(r.Header.Get("Authorization")))
		stub.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	tables := map[string]omnisdk.Table{}
	for _, j := range q.From() {
		_, rest, _ := strings.Cut(j.Resource().Handle(), ".")
		tbl, err := omnisdk.DescribeTable(corpus, "stackql_unstable_aws."+rest)
		if err != nil {
			t.Fatalf("describe: %v", err)
		}
		tables[j.Resource().Alias()] = tbl
	}
	res, err := omnisdk.Resolve(q, tables)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, res.Graph(), omnisdk.Args{Endpoint: srv.URL, Params: res.Params()})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var cols []string
		for k, v := range rows.Row() {
			cols = append(cols, fmt.Sprintf("%s=%v", k, v))
		}
		sort.Strings(cols)
		got = append(got, strings.Join(cols, ","))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(got)
	made := seen.matching("")
	sort.Strings(made)
	return got, made
}

func mustQuery(t *testing.T, from []query.Join, where []query.Predicate, sel []query.Output) query.Unresolved {
	t.Helper()
	q, err := query.New(from, where, sel)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return q
}

// SELECT u.UserName FROM aws.iam.users u WHERE region = 'us-east-1' AND u.UserName <> 'bob'
// <> binds nothing, so it is a filter on the rows.
func TestFilterThatCannotBind(t *testing.T) {
	q := mustQuery(t,
		[]query.Join{query.NewJoin(query.NewResource("u", "aws.iam.users"), query.Base)},
		[]query.Predicate{
			query.NewEq(query.NewColumn("", "region"), query.NewLiteral("us-east-1")),
			query.NewCompare(query.Ne, query.NewColumn("u", "UserName"), query.NewLiteral("bob")),
		},
		[]query.Output{query.NewOutput("UserName", query.NewColumn("u", "UserName"))},
	)
	rows, _ := runQuery(t, q)
	if want := []string{"UserName=alice"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
}

// SELECT u.UserName FROM aws.iam.users u WHERE region IN ('us-east-1', 'us-west-2')
// The query runs once per region, each request signed into its own.
func TestInListFansOut(t *testing.T) {
	q := mustQuery(t,
		[]query.Join{query.NewJoin(query.NewResource("u", "aws.iam.users"), query.Base)},
		[]query.Predicate{query.NewIn(query.NewColumn("", "region"),
			query.NewCollection(query.NewLiteral("us-east-1"), query.NewLiteral("us-west-2")))},
		[]query.Output{query.NewOutput("UserName", query.NewColumn("u", "UserName"))},
	)
	rows, made := runQuery(t, q)
	if want := []string{"UserName=alice", "UserName=alice", "UserName=bob", "UserName=bob"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	if want := []string{"ListUsers|us-east-1", "ListUsers|us-west-2"}; !reflect.DeepEqual(made, want) {
		t.Errorf("calls = %v, want %v", made, want)
	}
}

// SELECT a.UserName AS a, b.UserName AS b FROM aws.iam.users a INNER JOIN aws.iam.users b
// ON a.UserName = b.UserName WHERE region = 'us-east-1'
// Both sides list alone, so the join is local — and b is listed once, not once per row of a.
func TestJoinNeitherSideNeeds(t *testing.T) {
	q := mustQuery(t,
		[]query.Join{
			query.NewJoin(query.NewResource("a", "aws.iam.users"), query.Base),
			query.NewJoin(query.NewResource("b", "aws.iam.users"), query.Inner,
				query.NewEq(query.NewColumn("a", "UserName"), query.NewColumn("b", "UserName"))),
		},
		[]query.Predicate{query.NewEq(query.NewColumn("", "region"), query.NewLiteral("us-east-1"))},
		[]query.Output{
			query.NewOutput("a", query.NewColumn("a", "UserName")),
			query.NewOutput("b", query.NewColumn("b", "UserName")),
		},
	)
	rows, made := runQuery(t, q)
	if want := []string{"a=alice,b=alice", "a=bob,b=bob"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	if want := []string{"ListUsers|us-east-1", "ListUsers|us-east-1"}; !reflect.DeepEqual(made, want) {
		t.Errorf("calls = %v, want one listing per reference", made)
	}
}

// SELECT u.UserName FROM aws.iam.users u WHERE region = 'us-east-1' AND u.UserName IN ('alice', 'bob')
// UserName is a parameter of get_user, so the table runs once per name rather than listing.
func TestInListOnATableParameterFansOut(t *testing.T) {
	q := mustQuery(t,
		[]query.Join{query.NewJoin(query.NewResource("u", "aws.iam.users"), query.Base)},
		[]query.Predicate{
			query.NewEq(query.NewColumn("", "region"), query.NewLiteral("us-east-1")),
			query.NewIn(query.NewColumn("u", "UserName"),
				query.NewCollection(query.NewLiteral("alice"), query.NewLiteral("bob"))),
		},
		[]query.Output{query.NewOutput("UserName", query.NewColumn("u", "UserName"))},
	)
	rows, made := runQuery(t, q)
	if want := []string{"UserName=alice", "UserName=bob"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	if want := []string{"GetUser|us-east-1", "GetUser|us-east-1"}; !reflect.DeepEqual(made, want) {
		t.Errorf("calls = %v, want one GetUser per name", made)
	}
}
