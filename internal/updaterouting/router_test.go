package updaterouting_test

import (
	"context"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/updaterouting"
	"slices"
	"testing"
)

type rule struct {
	resource, target, source string
	priority, order          int
	requires, permits        []string
}

func (r rule) Resource() string   { return r.resource }
func (r rule) Target() string     { return r.target }
func (r rule) Priority() int      { return r.priority }
func (r rule) Requires() []string { return r.requires }
func (r rule) Permits() []string  { return r.permits }
func (r rule) Source() string     { return r.source }
func (r rule) Location() string   { return r.target }
func (r rule) Order() int         { return r.order }
func TestAttributeSetPredicate(t *testing.T) {
	r := rule{resource: "r", target: "update", requires: []string{"network"}, permits: []string{"network", "subnet", "tags"}}
	for _, tc := range []struct {
		name    string
		changed []string
		match   bool
	}{
		{"required missing", []string{"tags"}, false}, {"outside permits", []string{"network", "unknown"}, false}, {"exact", []string{"network"}, true}, {"optional", []string{"network", "tags"}, true}, {"empty", nil, false}, {"duplicates form a set", []string{"network", "network"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := updaterouting.New().Candidates(context.Background(), map[string][]string{"r": tc.changed}, []facade.UpdateRoute{r})
			if err != nil || (len(out) > 0) != tc.match {
				t.Fatalf("candidates=%v err=%v", out, err)
			}
		})
	}
	r.requires = nil
	out, err := updaterouting.New().Candidates(context.Background(), map[string][]string{"r": {"tags"}}, []facade.UpdateRoute{r})
	if err != nil || len(out) != 1 {
		t.Fatalf("empty requires: %v %v", out, err)
	}
}
func TestOrderingAndIndependentResources(t *testing.T) {
	rules := []facade.UpdateRoute{rule{resource: "b", target: "last", source: "b.yaml", priority: 1, permits: []string{"x"}}, rule{resource: "a", target: "second", source: "a.yaml", priority: 1, order: 2, permits: []string{"x"}}, rule{resource: "a", target: "first", source: "z.yaml", priority: 2, permits: []string{"x"}}, rule{resource: "a", target: "third", source: "a.yaml", priority: 1, order: 3, permits: []string{"x"}}, rule{resource: "b", target: "must-not-union", priority: 99, requires: []string{"x", "y"}, permits: []string{"x", "y"}}}
	for n := 0; n < 10; n++ {
		slices.Reverse(rules)
		changes := map[string][]string{}
		changes["b"] = []string{"x"}
		changes["a"] = []string{"x"}
		changes["unrelated"] = []string{"y"}
		out, err := updaterouting.New().Candidates(context.Background(), changes, rules)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, r := range out {
			names = append(names, r.Target())
		}
		if !slices.Equal(names, []string{"first", "second", "third", "last"}) {
			t.Fatal(names)
		}
	}
}
func TestInvalidRouteAndCancellation(t *testing.T) {
	for _, r := range []rule{{resource: "r", target: "x", requires: []string{"missing"}}, {resource: "r", target: "x", permits: []string{"x", "x"}}, {resource: "r", target: "x", permits: []string{""}}, {target: "x"}, {resource: "r"}} {
		if _, err := updaterouting.New().Candidates(context.Background(), nil, []facade.UpdateRoute{r}); err == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := updaterouting.New().Candidates(ctx, nil, nil); err == nil {
		t.Fatal("ignored cancellation")
	}
}
