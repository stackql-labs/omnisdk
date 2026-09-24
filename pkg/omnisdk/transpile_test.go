package omnisdk_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
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
The same query as a SQL front end would hand it over, in three steps.

 1. The parse: table references with their aliases, the ON and WHERE conjuncts, the select list.
    Nothing a document would have to say.
 2. The signatures: each table's select methods, their parameters and row columns, from omnisdk.
 3. Execution: Analyze combines the two into the graph, which runs.
*/
func TestParsedQueryDescribedThenRun(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	srv := iamStub(t)
	defer srv.Close()

	// 1. SELECT u.UserName, p.PolicyName
	//    FROM aws.iam.users u INNER JOIN aws.iam.attached_user_policies p ON p.UserName = u.UserName
	//    WHERE region = 'us-east-1'
	q := omnisdk.NewQuery(
		[]omnisdk.Node{
			omnisdk.NewNode("u", iamUsers, nil),
			omnisdk.NewNode("p", iamAttachedPolicies, nil),
		},
		[]omnisdk.Expression{
			omnisdk.NewEquals(omnisdk.NewColumn("p", "UserName"), omnisdk.NewColumn("u", "UserName")),
			omnisdk.NewEquals(omnisdk.NewField("region"), omnisdk.NewLiteral("us-east-1")),
		},
		[]omnisdk.SelectColumn{
			omnisdk.NewSelectColumn("UserName", omnisdk.NewColumn("u", "UserName")),
			omnisdk.NewSelectColumn("PolicyName", omnisdk.NewColumn("p", "PolicyName")),
		},
	)

	// 2. What the documents say about each table.
	described := map[string][]omnisdk.MethodSignature{}
	for _, n := range q.Nodes() {
		ms, err := omnisdk.Describe(corpus, n.Address())
		if err != nil {
			t.Fatalf("describe %s: %v", n.Alias(), err)
		}
		described[n.Alias()] = ms
	}
	// list_attached_user_policies requires UserName and users lists without it: that asymmetry is
	// the join's direction, and it is in the documents, not the query.
	if m := described["p"]; len(m) != 1 || !requires(m[0], "UserName") {
		t.Fatalf("p: want one method requiring UserName, got %v", methods(m))
	}

	// 3. Resolve and run.
	a, err := omnisdk.Analyze(q, described)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	pl, err := omnisdk.NewGraphSelectQuery(corpus, a.Graph(), omnisdk.Args{Endpoint: srv.URL, Params: a.Params()})
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

func requires(m omnisdk.MethodSignature, name string) bool {
	for _, p := range m.Params() {
		if p.Name() == name && p.Required() {
			return true
		}
	}
	return false
}

func methods(ms []omnisdk.MethodSignature) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Method())
	}
	return out
}
