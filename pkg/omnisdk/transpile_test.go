package omnisdk_test

import (
	"reflect"
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
