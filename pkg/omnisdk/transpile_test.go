package omnisdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
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

// withoutPolicies names a user the IAM stub reports no attached policies for; empty means none.
var withoutPolicies string

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
		case "CreateUser":
			if r.Form.Get("UserName") == "taken" {
				http.Error(w, "EntityAlreadyExists", http.StatusConflict)
				return
			}
			if r.Form.Get("UserName") == "fail" {
				http.Error(w, "internal", http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(w, `<CreateUserResponse><CreateUserResult><User><UserName>%s</UserName><Arn>arn:aws:iam::1:user/%s</Arn></User></CreateUserResult></CreateUserResponse>`,
				r.Form.Get("UserName"), r.Form.Get("UserName"))
		case "UpdateUser":
			fmt.Fprint(w, `<UpdateUserResponse><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></UpdateUserResponse>`)
		case "DeleteUser":
			fmt.Fprint(w, `<DeleteUserResponse><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></DeleteUserResponse>`)
		case "GetUser":
			fmt.Fprintf(w, `<GetUserResponse><GetUserResult><User><UserName>%s</UserName></User></GetUserResult></GetUserResponse>`,
				r.Form.Get("UserName"))
		case "ListAttachedUserPolicies":
			if r.Form.Get("UserName") == withoutPolicies {
				fmt.Fprint(w, `<ListAttachedUserPoliciesResponse><ListAttachedUserPoliciesResult><AttachedPolicies/>`+
					`</ListAttachedUserPoliciesResult></ListAttachedUserPoliciesResponse>`)
				return
			}
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
	rows, made, err := tryQuery(t, q)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	return rows, made
}

// tryQuery is runQuery returning the run's error rather than failing on it.
func tryQuery(t *testing.T, q query.Unresolved) (got []string, made []string, runErr error) {
	t.Helper()
	return tryQueryWith(t, q, nil, nil)
}

// tryQueryWith is tryQuery with the run's Args adjusted, and each request shown to seen before it
// is answered.
func tryQueryWith(t *testing.T, q query.Unresolved, adjust func(*omnisdk.Args), onRequest func(*http.Request)) (got []string, made []string, runErr error) {
	t.Helper()
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	var seen calls
	stub := iamStub(t)
	defer stub.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if onRequest != nil {
			onRequest(r)
		}
		call := r.Form.Get("Action") + "|" + signingRegion(r.Header.Get("Authorization"))
		if strings.HasSuffix(r.Form.Get("Action"), "User") && r.Form.Get("Action") != "GetUser" {
			call += "|" + r.Form.Get("UserName") + r.Form.Get("NewPath")
		}
		seen.add(call)
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
	if tg := q.Target(); tg != nil {
		_, rest, _ := strings.Cut(tg.Resource().Handle(), ".")
		tbl, err := omnisdk.DescribeMutation(corpus, "stackql_unstable_aws."+rest, tg.Verb().String())
		if err != nil {
			t.Fatalf("describe target: %v", err)
		}
		tables[tg.Resource().Alias()] = tbl
	}
	res, err := omnisdk.Resolve(q, tables)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	args := omnisdk.Args{Endpoint: srv.URL, Params: res.Params()}
	if adjust != nil {
		adjust(&args)
	}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, res.Graph(), args)
	if err != nil {
		return nil, nil, err
	}
	rows, err := pl.Open(context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cols []string
		for k, v := range rows.Row() {
			cols = append(cols, fmt.Sprintf("%s=%v", k, v))
		}
		sort.Strings(cols)
		got = append(got, strings.Join(cols, ","))
	}
	runErr = rows.Err()
	sort.Strings(got)
	made = seen.matching("")
	sort.Strings(made)
	return got, made, runErr
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

// SELECT u.UserName, p.PolicyName FROM aws.iam.users u INNER JOIN aws.iam.attached_user_policies p
// ON p.UserName = split_part(u.UserName, 'l', 1) WHERE region = 'us-east-1'
// The join value is computed by the producer before it travels the edge: alice → "a", bob → "bob".
func TestJoinOnAComputedValue(t *testing.T) {
	q := mustQuery(t,
		[]query.Join{
			query.NewJoin(query.NewResource("u", "aws.iam.users"), query.Base),
			query.NewJoin(query.NewResource("p", "aws.iam.attached_user_policies"), query.Inner,
				query.NewEq(query.NewColumn("p", "UserName"),
					query.NewCall("split_part", query.NewColumn("u", "UserName"), query.NewLiteral("l"), query.NewLiteral(1)))),
		},
		[]query.Predicate{query.NewEq(query.NewColumn("", "region"), query.NewLiteral("us-east-1"))},
		[]query.Output{
			query.NewOutput("UserName", query.NewColumn("u", "UserName")),
			query.NewOutput("PolicyName", query.NewColumn("p", "PolicyName")),
		},
	)
	rows, _ := runQuery(t, q)
	if want := []string{"PolicyName=a-policy,UserName=alice", "PolicyName=bob-policy,UserName=bob"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
}

// SELECT u.UserName, split_part(p.PolicyName, u.UserName, 2) AS suffix FROM ... ON p.UserName = u.UserName
// An output reading two tables is computed on the finished row.
func TestOutputAcrossTables(t *testing.T) {
	q := mustQuery(t,
		[]query.Join{
			query.NewJoin(query.NewResource("u", "aws.iam.users"), query.Base),
			query.NewJoin(query.NewResource("p", "aws.iam.attached_user_policies"), query.Inner,
				query.NewEq(query.NewColumn("p", "UserName"), query.NewColumn("u", "UserName"))),
		},
		[]query.Predicate{query.NewEq(query.NewColumn("", "region"), query.NewLiteral("us-east-1"))},
		[]query.Output{
			query.NewOutput("UserName", query.NewColumn("u", "UserName")),
			query.NewOutput("suffix", query.NewCall("split_part",
				query.NewColumn("p", "PolicyName"), query.NewColumn("u", "UserName"), query.NewLiteral(2))),
		},
	)
	rows, _ := runQuery(t, q)
	if want := []string{"UserName=alice,suffix=-policy", "UserName=bob,suffix=-policy"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
}

// rowKeys are the column names of a runQuery row.
func rowKeys(row string) []string {
	var out []string
	for _, kv := range strings.Split(row, ",") {
		k, _, _ := strings.Cut(kv, "=")
		out = append(out, k)
	}
	return out
}

var usersColumns = []string{"Arn", "CreateDate", "PasswordLastUsed", "Path", "PermissionsBoundary", "Tags", "UserId", "UserName"}

// SELECT * FROM aws.iam.users u WHERE region = 'us-east-1'
// The star is the columns the document declares for the row.
func TestSelectStar(t *testing.T) {
	q := mustQuery(t,
		[]query.Join{query.NewJoin(query.NewResource("u", "aws.iam.users"), query.Base)},
		[]query.Predicate{query.NewEq(query.NewColumn("", "region"), query.NewLiteral("us-east-1"))},
		[]query.Output{query.NewOutput("", query.NewStar(""))},
	)
	rows, _ := runQuery(t, q)
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want alice and bob", rows)
	}
	for _, r := range rows {
		if got := rowKeys(r); !reflect.DeepEqual(got, usersColumns) {
			t.Errorf("columns = %v, want %v", got, usersColumns)
		}
	}
	if !strings.Contains(rows[0], "UserName=alice") || !strings.Contains(rows[1], "UserName=bob") {
		t.Errorf("rows = %v", rows)
	}
}

// SELECT u.*, p.PolicyName FROM aws.iam.users u INNER JOIN aws.iam.attached_user_policies p ON ...
func TestQualifiedStarBesideAColumn(t *testing.T) {
	q := mustQuery(t,
		[]query.Join{
			query.NewJoin(query.NewResource("u", "aws.iam.users"), query.Base),
			query.NewJoin(query.NewResource("p", "aws.iam.attached_user_policies"), query.Inner,
				query.NewEq(query.NewColumn("p", "UserName"), query.NewColumn("u", "UserName"))),
		},
		[]query.Predicate{query.NewEq(query.NewColumn("", "region"), query.NewLiteral("us-east-1"))},
		[]query.Output{
			query.NewOutput("", query.NewStar("u")),
			query.NewOutput("PolicyName", query.NewColumn("p", "PolicyName")),
		},
	)
	rows, _ := runQuery(t, q)
	want := append([]string{"PolicyName"}, usersColumns...)
	sort.Strings(want)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	for _, r := range rows {
		if got := rowKeys(r); !reflect.DeepEqual(got, want) {
			t.Errorf("columns = %v, want %v", got, want)
		}
	}
}

// SELECT * over a self-join names every column twice: ambiguous, so resolution refuses it.
func TestStarOverASelfJoinIsAmbiguous(t *testing.T) {
	requireCorpus(t)
	q := mustQuery(t,
		[]query.Join{
			query.NewJoin(query.NewResource("a", "aws.iam.users"), query.Base),
			query.NewJoin(query.NewResource("b", "aws.iam.users"), query.Inner,
				query.NewEq(query.NewColumn("a", "UserName"), query.NewColumn("b", "UserName"))),
		},
		nil,
		[]query.Output{query.NewOutput("", query.NewStar(""))},
	)
	tbl, err := omnisdk.DescribeTable(corpus, iamUsers)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if _, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"a": tbl, "b": tbl}); err == nil {
		t.Error("resolved a star whose columns collide")
	}
}

func users(alias string) query.Resource { return query.NewResource(alias, "aws.iam.users") }

func regionEq() query.Predicate {
	return query.NewEq(query.NewColumn("", "region"), query.NewLiteral("us-east-1"))
}

func mustMutation(t *testing.T, tg query.Target, from []query.Join, where []query.Predicate, ret []query.Output) query.Unresolved {
	t.Helper()
	q, err := query.NewMutation(tg, from, where, ret)
	if err != nil {
		t.Fatalf("mutation: %v", err)
	}
	return q
}

// INSERT INTO aws.iam.users (UserName) VALUES ('carol') RETURNING UserName, Arn
func TestInsertValuesReturning(t *testing.T) {
	q := mustMutation(t,
		query.NewInsert(users("t"), query.NewAssignment("UserName", query.NewLiteral("carol"))),
		nil, []query.Predicate{regionEq()},
		[]query.Output{
			query.NewOutput("UserName", query.NewColumn("t", "UserName")),
			query.NewOutput("Arn", query.NewColumn("t", "Arn")),
		},
	)
	rows, made := runQuery(t, q)
	if want := []string{"Arn=arn:aws:iam::1:user/carol,UserName=carol"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	if want := []string{"CreateUser|us-east-1|carol"}; !reflect.DeepEqual(made, want) {
		t.Errorf("calls = %v, want %v", made, want)
	}
}

// INSERT INTO aws.iam.users (UserName) SELECT split_part(u.UserName, 'l', 1) FROM aws.iam.users u
// One effect per source row, each value computed on the source before it travels the edge.
func TestInsertSelect(t *testing.T) {
	q := mustMutation(t,
		query.NewInsert(users("t"), query.NewAssignment("UserName",
			query.NewCall("split_part", query.NewColumn("u", "UserName"), query.NewLiteral("l"), query.NewLiteral(1)))),
		[]query.Join{query.NewJoin(users("u"), query.Base)},
		[]query.Predicate{regionEq()},
		nil,
	)
	_, made := runQuery(t, q)
	want := []string{"CreateUser|us-east-1|a", "CreateUser|us-east-1|bob", "ListUsers|us-east-1"}
	if !reflect.DeepEqual(made, want) {
		t.Errorf("calls = %v, want %v", made, want)
	}
}

// UPDATE aws.iam.users SET NewPath = '/x/' WHERE UserName IN ('alice', 'bob')
func TestUpdateFansOutOverItsWhere(t *testing.T) {
	q := mustMutation(t,
		query.NewUpdate(users("t"), query.NewAssignment("NewPath", query.NewLiteral("/x/"))),
		nil,
		[]query.Predicate{regionEq(), query.NewIn(query.NewColumn("t", "UserName"),
			query.NewCollection(query.NewLiteral("alice"), query.NewLiteral("bob")))},
		nil,
	)
	_, made := runQuery(t, q)
	if want := []string{"UpdateUser|us-east-1|alice/x/", "UpdateUser|us-east-1|bob/x/"}; !reflect.DeepEqual(made, want) {
		t.Errorf("calls = %v, want %v", made, want)
	}
}

// DELETE FROM aws.iam.users WHERE UserName = 'alice'
func TestDelete(t *testing.T) {
	q := mustMutation(t, query.NewDelete(users("t")), nil,
		[]query.Predicate{regionEq(), query.NewEq(query.NewColumn("", "UserName"), query.NewLiteral("alice"))},
		nil,
	)
	_, made := runQuery(t, q)
	if want := []string{"DeleteUser|us-east-1|alice"}; !reflect.DeepEqual(made, want) {
		t.Errorf("calls = %v, want %v", made, want)
	}
}

// A failed effect is sent once, never retried, and reported as possibly applied.
func TestFailedEffectIsNotRetried(t *testing.T) {
	q := mustMutation(t,
		query.NewInsert(users("t"), query.NewAssignment("UserName", query.NewLiteral("fail"))),
		nil, []query.Predicate{regionEq()}, nil,
	)
	_, made, err := tryQuery(t, q)
	if err == nil || !strings.Contains(err.Error(), "may or may not have taken effect") || !errors.Is(err, omnisdk.ErrOutcomeUnknown) {
		t.Errorf("err = %v, want the outcome reported as unknown", err)
	}
	if want := []string{"CreateUser|us-east-1|fail"}; !reflect.DeepEqual(made, want) {
		t.Errorf("calls = %v, want exactly one attempt", made)
	}
}

// DELETE FROM aws.iam.users WHERE Path <> '/keep/'
// Path is no parameter of delete_user: the condition could only be checked after the effect.
func TestMutationRefusesAConditionItCannotTake(t *testing.T) {
	requireCorpus(t)
	q := mustMutation(t, query.NewDelete(users("t")), nil,
		[]query.Predicate{query.NewCompare(query.Ne, query.NewColumn("t", "Path"), query.NewLiteral("/keep/"))},
		nil,
	)
	tbl, err := omnisdk.DescribeMutation(corpus, iamUsers, "delete")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if _, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"t": tbl}); err == nil {
		t.Error("resolved a delete whose condition would apply after the effect")
	}
}

// INSERT INTO google.storage.buckets (project, name, location) VALUES ('demo', 'my-bucket', 'US')
// RETURNING name, location
// project is a declared query parameter. name and location are declared nowhere: the method takes a
// JSON body, so they are its fields. RETURNING reads the created bucket.
func TestInsertIntoAJSONBody(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	var method, rawQuery, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		method, rawQuery, body = r.Method, r.URL.RawQuery, string(b)
		fmt.Fprint(w, `{"kind":"storage#bucket","id":"my-bucket","name":"my-bucket","location":"US"}`)
	}))
	defer srv.Close()

	const address = "stackql_unstable_google.storage.buckets"
	q := mustMutation(t,
		query.NewInsert(query.NewResource("b", "google.storage.buckets"),
			query.NewAssignment("project", query.NewLiteral("demo")),
			query.NewAssignment("name", query.NewLiteral("my-bucket")),
			query.NewAssignment("location", query.NewLiteral("US")),
		),
		nil, nil,
		[]query.Output{
			query.NewOutput("name", query.NewColumn("b", "name")),
			query.NewOutput("location", query.NewColumn("b", "location")),
		},
	)
	tbl, err := omnisdk.DescribeMutation(corpus, address, "insert")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	res, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"b": tbl})
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
	var got []omnisdk.Row
	for rows.Next() {
		got = append(got, rows.Row())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("body %q is not JSON: %v", body, err)
	}
	if sent["name"] != "my-bucket" || sent["location"] != "US" {
		t.Errorf("body = %s, want name and location as its fields", body)
	}
	if !strings.Contains(rawQuery, "project=demo") || strings.Contains(rawQuery, "my-bucket") {
		t.Errorf("query = %q, want project there and the body fields not", rawQuery)
	}
	if len(got) != 1 || got[0]["name"] != "my-bucket" || got[0]["location"] != "US" {
		t.Errorf("rows = %v, want the created bucket", got)
	}
}

func insertCarol(t *testing.T) query.Unresolved {
	return mustMutation(t,
		query.NewInsert(users("t"), query.NewAssignment("UserName", query.NewLiteral("carol"))),
		nil, []query.Predicate{regionEq()}, nil,
	)
}

// With a journal, the effect's intent is on disk before its request reaches the provider.
func TestJournaledEffectIsRecordedBeforeItsRequest(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "journal", "run-1.jsonl")
	var atRequest string
	_, made, err := tryQueryWith(t, insertCarol(t),
		func(a *omnisdk.Args) { a.Journal = &omnisdk.Journal{State: state, RunID: "run-1"} },
		func(r *http.Request) {
			if r.Form.Get("Action") == "CreateUser" {
				b, _ := os.ReadFile(path)
				atRequest = string(b)
			}
		})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := []string{"CreateUser|us-east-1|carol"}; !reflect.DeepEqual(made, want) {
		t.Errorf("calls = %v, want %v", made, want)
	}
	if !strings.Contains(atRequest, `"t/`) {
		t.Fatalf("journal at request time = %q, want the intent already recorded", atRequest)
	}
	if !strings.Contains(atRequest, "stackql_unstable_aws.iam.users:insert") {
		t.Errorf("journal = %q, want the insert exchange named", atRequest)
	}
}

// If the intent cannot be recorded, the request is never made.
func TestUnrecordableEffectIsNotAttempted(t *testing.T) {
	state := t.TempDir()
	// The run's journal file cannot be opened for append: a directory stands where it would be.
	if err := os.MkdirAll(filepath.Join(state, "journal", "run-1.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, made, err := tryQueryWith(t, insertCarol(t),
		func(a *omnisdk.Args) { a.Journal = &omnisdk.Journal{State: state, RunID: "run-1"} }, nil)
	if err == nil || !strings.Contains(err.Error(), "was not attempted") || !errors.Is(err, omnisdk.ErrNotAttempted) {
		t.Errorf("err = %v, want the effect reported as not attempted", err)
	}
	if len(made) != 0 {
		t.Errorf("calls = %v, want none", made)
	}
}

// Opting in without naming the run is refused: which run a record belongs to is scope.
func TestJournalNeedsARunID(t *testing.T) {
	_, _, err := tryQueryWith(t, insertCarol(t),
		func(a *omnisdk.Args) { a.Journal = &omnisdk.Journal{State: t.TempDir()} }, nil)
	if err == nil || !strings.Contains(err.Error(), "run id") {
		t.Errorf("err = %v, want a missing run id refused", err)
	}
}

// A 4xx is the provider refusing the request: the effect did not happen, and is reported so.
func TestRejectedEffectIsReportedAsRejected(t *testing.T) {
	q := mustMutation(t,
		query.NewInsert(users("t"), query.NewAssignment("UserName", query.NewLiteral("taken"))),
		nil, []query.Predicate{regionEq()}, nil,
	)
	_, _, err := tryQuery(t, q)
	if err == nil || !strings.Contains(err.Error(), "was rejected") || !errors.Is(err, omnisdk.ErrRejected) {
		t.Errorf("err = %v, want the effect reported as rejected", err)
	}
}

// SELECT u.UserName, p.PolicyName FROM aws.iam.users u LEFT JOIN aws.iam.attached_user_policies p
// ON p.UserName = u.UserName WHERE region = 'us-east-1'
// bob has no policies: his row stays, without a PolicyName.
func TestLeftJoinKeepsTheUnmatched(t *testing.T) {
	withoutPolicies = "bob"
	defer func() { withoutPolicies = "" }()
	q := mustQuery(t,
		[]query.Join{
			query.NewJoin(users("u"), query.Base),
			query.NewJoin(query.NewResource("p", "aws.iam.attached_user_policies"), query.Left,
				query.NewEq(query.NewColumn("p", "UserName"), query.NewColumn("u", "UserName"))),
		},
		[]query.Predicate{regionEq()},
		[]query.Output{
			query.NewOutput("UserName", query.NewColumn("u", "UserName")),
			query.NewOutput("PolicyName", query.NewColumn("p", "PolicyName")),
		},
	)
	rows, _ := runQuery(t, q)
	if want := []string{"PolicyName=alice-policy,UserName=alice", "UserName=bob"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
}

// SELECT a.UserName AS a, b.UserName AS b FROM aws.iam.users a LEFT JOIN aws.iam.users b
// ON b.UserName = a.UserName AND b.UserName <> 'bob' WHERE region = 'us-east-1'
// The <> is part of the match, not a filter: bob stays, unmatched. b is listed once.
func TestLeftJoinConditionDecidesTheMatch(t *testing.T) {
	q := mustQuery(t,
		[]query.Join{
			query.NewJoin(users("a"), query.Base),
			query.NewJoin(users("b"), query.Left,
				query.NewEq(query.NewColumn("b", "UserName"), query.NewColumn("a", "UserName")),
				query.NewCompare(query.Ne, query.NewColumn("b", "UserName"), query.NewLiteral("bob"))),
		},
		[]query.Predicate{regionEq()},
		[]query.Output{
			query.NewOutput("a", query.NewColumn("a", "UserName")),
			query.NewOutput("b", query.NewColumn("b", "UserName")),
		},
	)
	rows, made := runQuery(t, q)
	if want := []string{"a=alice,b=alice", "a=bob"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
	if want := []string{"ListUsers|us-east-1", "ListUsers|us-east-1"}; !reflect.DeepEqual(made, want) {
		t.Errorf("calls = %v, want one listing per reference", made)
	}
}

// FROM aws.iam.attached_user_policies p LEFT JOIN aws.iam.users u ON p.UserName = u.UserName
// p is preserved but cannot list without u's UserName: nothing to preserve.
func TestLeftJoinRefusesAPreservedSideThatNeedsTheOther(t *testing.T) {
	requireCorpus(t)
	q := mustQuery(t,
		[]query.Join{
			query.NewJoin(query.NewResource("p", "aws.iam.attached_user_policies"), query.Base),
			query.NewJoin(users("u"), query.Left,
				query.NewEq(query.NewColumn("p", "UserName"), query.NewColumn("u", "UserName"))),
		},
		nil, nil,
	)
	pt, err := omnisdk.DescribeTable(corpus, iamAttachedPolicies)
	if err != nil {
		t.Fatal(err)
	}
	ut, err := omnisdk.DescribeTable(corpus, iamUsers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"p": pt, "u": ut}); err == nil {
		t.Error("resolved a left join whose preserved side needs the joined one")
	}
}

// INSERT INTO aws.iam.users (UserName) VALUES ('carol'), ('dave') — one effect per row.
func TestInsertSeveralValuesRows(t *testing.T) {
	row := func(name string) []query.Assignment {
		return []query.Assignment{query.NewAssignment("UserName", query.NewLiteral(name))}
	}
	q := mustMutation(t, query.NewInsertRows(users("t"), row("carol"), row("dave")), nil, []query.Predicate{regionEq()}, nil)
	_, made := runQuery(t, q)
	if want := []string{"CreateUser|us-east-1|carol", "CreateUser|us-east-1|dave"}; !reflect.DeepEqual(made, want) {
		t.Errorf("calls = %v, want %v", made, want)
	}
}

// INSERT INTO google.storage.buckets (project, name, location) VALUES ('demo','a','US'), ('demo','b','EU')
// Each row's body fields go together, in its own request.
func TestInsertSeveralRowsIntoAJSONBody(t *testing.T) {
	requireCorpus(t)
	t.Setenv("GOOGLE_CREDENTIALS", serviceAccountKey(t))
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/token") {
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.URL.Query().Get("project")+" "+string(b))
		mu.Unlock()
		fmt.Fprint(w, `{"kind":"storage#bucket"}`)
	}))
	defer srv.Close()
	row := func(name, loc string) []query.Assignment {
		return []query.Assignment{
			query.NewAssignment("project", query.NewLiteral("demo")),
			query.NewAssignment("name", query.NewLiteral(name)),
			query.NewAssignment("location", query.NewLiteral(loc)),
		}
	}
	q := mustMutation(t, query.NewInsertRows(query.NewResource("b", "google.storage.buckets"), row("a", "US"), row("b", "EU")), nil, nil, nil)
	tbl, err := omnisdk.DescribeMutation(corpus, "stackql_unstable_google.storage.buckets", "insert")
	if err != nil {
		t.Fatal(err)
	}
	res, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"b": tbl})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, res.Graph(), omnisdk.Args{Endpoint: srv.URL})
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
	sort.Strings(bodies)
	want := []string{`demo {"location":"EU","name":"b"}`, `demo {"location":"US","name":"a"}`}
	if !reflect.DeepEqual(bodies, want) {
		t.Errorf("requests = %v, want %v", bodies, want)
	}
}

// SELECT u.UserName, upper(u.UserName) AS shout FROM aws.iam.users u
// WHERE region = 'us-east-1' AND regexp_like(u.UserName, '^A', 'i') AND json_extract('{"k":1}', '$.k') = 1
// Catalogue functions project and filter like any other.
func TestCatalogueFunctionsInAQuery(t *testing.T) {
	q := mustQuery(t,
		[]query.Join{query.NewJoin(users("u"), query.Base)},
		[]query.Predicate{
			regionEq(),
			query.NewTest(query.NewCall("regexp_like", query.NewColumn("u", "UserName"), query.NewLiteral("^A"), query.NewLiteral("i"))),
			query.NewEq(query.NewCall("json_extract", query.NewLiteral(`{"k":1}`), query.NewLiteral("$.k")), query.NewLiteral(1)),
		},
		[]query.Output{
			query.NewOutput("UserName", query.NewColumn("u", "UserName")),
			query.NewOutput("shout", query.NewCall("upper", query.NewColumn("u", "UserName"))),
		},
	)
	rows, _ := runQuery(t, q)
	if want := []string{"UserName=alice,shout=ALICE"}; !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %v, want %v", rows, want)
	}
}
