package omnisdk_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

type handleRows struct {
	rows   []omnisdk.Row
	index  int
	closed *atomic.Int32
	err    error
}

func (r *handleRows) Next() bool       { r.index++; return r.index <= len(r.rows) }
func (r *handleRows) Row() omnisdk.Row { return r.rows[r.index-1] }
func (r *handleRows) Err() error       { return r.err }
func (r *handleRows) Close() error {
	if r.closed != nil {
		r.closed.Add(1)
	}
	return nil
}

func TestRelationHandlesResolveAndBindDependentReads(t *testing.T) {
	var opened, closed atomic.Int32
	source, err := omnisdk.NewRelation("local.source", nil, map[string]omnisdk.ColumnType{"id": {Type: "string"}},
		func(context.Context, map[string]any) (omnisdk.Rows, error) {
			opened.Add(1)
			return &handleRows{rows: []omnisdk.Row{{"id": "a"}, {"id": "b"}}, closed: &closed}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := omnisdk.NewRelation("nested.details", []omnisdk.Param{{Name: "owner", Required: true}}, map[string]omnisdk.ColumnType{"id": {Type: "string"}},
		func(_ context.Context, inputs map[string]any) (omnisdk.Rows, error) {
			if len(inputs) != 1 {
				return nil, fmt.Errorf("undeclared inputs leaked: %v", inputs)
			}
			opened.Add(1)
			return &handleRows{rows: []omnisdk.Row{{"id": "detail-" + inputs["owner"].(string)}}, closed: &closed}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	q, err := query.New([]query.Join{
		query.NewJoin(query.NewResource("s", "local.source"), query.Base),
		query.NewJoin(query.NewResource("d", "nested.details"), query.Inner, query.NewCompare(query.Eq, query.NewColumn("d", "owner"), query.NewColumn("s", "id"))),
	}, nil, []query.Output{query.NewOutput("source", query.NewColumn("s", "id")), query.NewOutput("detail", query.NewColumn("d", "id"))})
	if err != nil {
		t.Fatal(err)
	}
	res, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"s": source, "d": detail})
	if err != nil {
		t.Fatal(err)
	}
	p, err := omnisdk.NewGraphQuery("not-a-registry", res.Graph(), omnisdk.Args{Params: res.Params(), Relations: map[string]omnisdk.Relation{source.Address(): source, detail.Address(): detail}})
	if err != nil {
		t.Fatal(err)
	}
	if opened.Load() != 0 {
		t.Fatal("planning performed IO")
	}
	rows, err := p.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []omnisdk.Row
	for rows.Next() {
		got = append(got, rows.Row())
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	want := []omnisdk.Row{{"source": "a", "detail": "detail-a"}, {"source": "b", "detail": "detail-b"}}
	// Independent dependent reads may complete in either order.
	sort.Slice(got, func(i, j int) bool { return got[i]["source"].(string) < got[j]["source"].(string) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v, want %v", got, want)
	}
	if opened.Load() != 3 || closed.Load() != 3 {
		t.Fatalf("opens=%d closes=%d", opened.Load(), closed.Load())
	}
}

func TestRelationHandlesPreserveErrorsAndRejectMutation(t *testing.T) {
	sentinel := errors.New("database read failed")
	relation, err := omnisdk.NewRelation("db.rows", nil, map[string]omnisdk.ColumnType{"id": {Type: "string"}}, func(context.Context, map[string]any) (omnisdk.Rows, error) {
		return &handleRows{rows: []omnisdk.Row{{"id": "first"}}, err: sentinel}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("r", relation.Address(), nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	args := omnisdk.Args{Relations: map[string]omnisdk.Relation{relation.Address(): relation}}
	p, err := omnisdk.NewGraphQuery("", g, args)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := p.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	completed := 0
	for rows.Next() {
		completed++
		if rows.Row()["id"] != "first" {
			t.Fatalf("completed row = %v", rows.Row())
		}
	}
	if completed != 1 {
		t.Fatalf("completed rows = %d, want one row before the error", completed)
	}
	if !errors.Is(rows.Err(), sentinel) {
		t.Fatalf("error=%v", rows.Err())
	}
	g, err = omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewMutationNode("r", relation.Address(), "delete", nil, nil, nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = omnisdk.NewGraphQuery("", g, args); err == nil {
		t.Fatal("mutation accepted for a read handle")
	}
}

func TestRelationHandlesRequireInputsAndHonorCanceledOpen(t *testing.T) {
	var opened atomic.Int32
	relation, err := omnisdk.NewRelation("db.scoped", []omnisdk.Param{{Name: "account", Required: true}},
		map[string]omnisdk.ColumnType{"id": {Type: "string"}},
		func(context.Context, map[string]any) (omnisdk.Rows, error) {
			opened.Add(1)
			return &handleRows{}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	args := omnisdk.Args{Relations: map[string]omnisdk.Relation{relation.Address(): relation}}
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("r", relation.Address(), nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = omnisdk.NewGraphQuery("", g, args); err == nil {
		t.Fatal("missing required input accepted")
	}
	g, err = omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("r", relation.Address(), map[string]string{"account": "explicit"})}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := omnisdk.NewGraphQuery("", g, args)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows, err := p.Open(ctx)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
		}
		err = rows.Err()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if opened.Load() != 0 {
		t.Fatal("canceled query opened the relation")
	}
}

func TestRelationReadProjectsConstantsAfterFiltering(t *testing.T) {
	relation, err := omnisdk.NewRelation("local.constants", nil, map[string]omnisdk.ColumnType{"id": {Type: "string"}},
		func(context.Context, map[string]any) (omnisdk.Rows, error) {
			return &handleRows{rows: []omnisdk.Row{{"id": "keep"}, {"id": "drop"}}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	q, err := query.New([]query.Join{query.NewJoin(query.NewResource("s", relation.Address()), query.Base)},
		[]query.Predicate{query.NewEq(query.NewColumn("s", "id"), query.NewLiteral("keep"))},
		[]query.Output{query.NewOutput("id", query.NewColumn("s", "id")), query.NewOutput("scope", query.NewLiteral("explicit")), query.NewOutput("empty", query.NewLiteral(nil))})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"s": relation})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := omnisdk.NewGraphQuery("", resolved.Graph(), omnisdk.Args{Params: resolved.Params(), Relations: map[string]omnisdk.Relation{relation.Address(): relation}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := plan.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() || !reflect.DeepEqual(rows.Row(), omnisdk.Row{"id": "keep", "scope": "explicit", "empty": nil}) {
		t.Fatalf("row=%v err=%v", rows.Row(), rows.Err())
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("continuation=%v", rows.Err())
	}
}

func TestRelationLeftAntiJoinPreservesUnmatchedSource(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			left, err := omnisdk.NewRelation("local.left", nil, map[string]omnisdk.ColumnType{"bucket": {Type: "string"}}, func(context.Context, map[string]any) (omnisdk.Rows, error) {
				return &handleRows{rows: []omnisdk.Row{{"bucket": "missing"}}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			right, err := omnisdk.NewRelation("local.right", nil, map[string]omnisdk.ColumnType{"name": {Type: "string"}}, func(context.Context, map[string]any) (omnisdk.Rows, error) {
				values := []omnisdk.Row{{"name": "other"}}
				if empty {
					values = nil
				}
				return &handleRows{rows: values}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			q, err := query.New([]query.Join{
				query.NewJoin(query.NewResource("l", left.Address()), query.Base),
				query.NewJoin(query.NewResource("r", right.Address()), query.Left, query.NewEq(query.NewColumn("l", "bucket"), query.NewColumn("r", "name"))),
			}, []query.Predicate{query.NewTest(query.NewCall("is_null", query.NewColumn("r", "name")))}, []query.Output{query.NewOutput("bucket", query.NewColumn("l", "bucket"))})
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"l": left, "r": right})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := omnisdk.NewGraphQuery("", resolved.Graph(), omnisdk.Args{Params: resolved.Params(), Relations: map[string]omnisdk.Relation{left.Address(): left, right.Address(): right}})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := plan.Open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("lost unmatched source: %v", rows.Err())
			}
			if rows.Row()["bucket"] != "missing" {
				t.Fatalf("row=%v", rows.Row())
			}
			if rows.Next() || rows.Err() != nil {
				t.Fatalf("continuation=%v", rows.Err())
			}

		})
	}
}
