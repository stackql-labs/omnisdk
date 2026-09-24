package query_test

import (
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/query"
)

// SELECT u.UserName, p.PolicyName
// FROM aws.iam.users u INNER JOIN aws.iam.attached_user_policies p ON p.UserName = u.UserName
// WHERE region = 'us-east-1'
func TestSimpleJoinIsStatedAsWritten(t *testing.T) {
	u := query.NewResource("u", "aws.iam.users")
	p := query.NewResource("p", "aws.iam.attached_user_policies")
	q, err := query.New(
		[]query.Join{
			query.NewJoin(u, query.Base),
			query.NewJoin(p, query.Inner,
				query.NewEq(query.NewColumn("p", "UserName"), query.NewColumn("u", "UserName"))),
		},
		[]query.Predicate{query.NewEq(query.NewColumn("", "region"), query.NewLiteral("us-east-1"))},
		[]query.Output{
			query.NewOutput("UserName", query.NewColumn("u", "UserName")),
			query.NewOutput("PolicyName", query.NewColumn("p", "PolicyName")),
		},
	)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if got := q.From()[1].Form(); got != query.Inner {
		t.Errorf("join form = %s", got)
	}
	// region is unqualified: ambiguous until the resources' columns are known.
	c := q.Where()[0].(query.Compare).Left().(query.Column)
	if c.Qualifier() != "" || c.Name() != "region" {
		t.Errorf("where column = %q.%q", c.Qualifier(), c.Name())
	}
}

func TestUnaliasedResourceIsItsHandle(t *testing.T) {
	if got := query.NewResource("", "aws.iam.users").Alias(); got != "aws.iam.users" {
		t.Errorf("alias = %q", got)
	}
}

// Each is wrong in the query itself, before any signature is consulted.
func TestNewRejectsWhatTheQueryGetsWrong(t *testing.T) {
	u := query.NewResource("u", "aws.iam.users")
	p := query.NewResource("p", "aws.iam.attached_user_policies")
	col := func(q, n string) query.Expr { return query.NewColumn(q, n) }
	cases := map[string]struct {
		from  []query.Join
		where []query.Predicate
		sel   []query.Output
	}{
		"no resources": {},
		"first is not the base": {
			from: []query.Join{query.NewJoin(u, query.Inner)},
		},
		"a later base": {
			from: []query.Join{query.NewJoin(u, query.Base), query.NewJoin(p, query.Base)},
		},
		"same handle twice, unaliased": {
			from: []query.Join{
				query.NewJoin(query.NewResource("", "aws.iam.users"), query.Base),
				query.NewJoin(query.NewResource("", "aws.iam.users"), query.Inner),
			},
		},
		"ON names a resource joined later": {
			from: []query.Join{
				query.NewJoin(u, query.Base, query.NewEq(col("u", "UserName"), col("p", "UserName"))),
				query.NewJoin(p, query.Inner),
			},
		},
		"WHERE names an unknown alias": {
			from:  []query.Join{query.NewJoin(u, query.Base)},
			where: []query.Predicate{query.NewEq(col("x", "UserName"), query.NewLiteral("a"))},
		},
		"output named twice": {
			from: []query.Join{query.NewJoin(u, query.Base), query.NewJoin(p, query.Inner)},
			sel: []query.Output{
				query.NewOutput("UserName", col("u", "UserName")),
				query.NewOutput("UserName", col("p", "UserName")),
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := query.New(tc.from, tc.where, tc.sel); err == nil {
				t.Error("accepted")
			}
		})
	}
}
