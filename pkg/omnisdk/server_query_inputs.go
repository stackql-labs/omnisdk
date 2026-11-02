package omnisdk

import (
	"fmt"
	"maps"
	"slices"

	"github.com/stackql-labs/omnisdk/pkg/query"
)

// BindServerInputs adds declared endpoint bindings without mutating the query.
// Hosts supply environment access explicitly; SQL input always takes precedence.
func BindServerInputs(q query.Unresolved, tables map[string]Table, lookup func(string) (string, bool)) (query.Unresolved, error) {
	where := slices.Clone(q.Where())
	for _, alias := range slices.Sorted(maps.Keys(tables)) {
		bindings, err := NewMethodInputResolver(tables[alias].Methods(), nil, lookup).Resolve(func(name string) bool {
			return explicitServerBinding(q, alias, name)
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", alias, err)
		}
		for _, name := range slices.Sorted(maps.Keys(bindings)) {
			where = append(where, query.NewEq(query.NewColumn(alias, name), query.NewLiteral(bindings[name])))
		}
	}
	return queryWithWhere(q, where)
}

func explicitServerBinding(q query.Unresolved, alias, name string) bool {
	matches := func(e query.Expr) bool {
		col, ok := e.(query.Column)
		return ok && col.Name() == name && (col.Qualifier() == "" || col.Qualifier() == alias)
	}
	predicates := slices.Clone(q.Where())
	for _, join := range q.From() {
		predicates = append(predicates, join.On()...)
	}
	for _, predicate := range predicates {
		switch p := predicate.(type) {
		case query.Compare:
			if p.Op() == query.Eq && (matches(p.Left()) || matches(p.Right())) {
				return true
			}
		case query.In:
			if matches(p.Expr()) {
				return true
			}
		}
	}
	target := q.Target()
	if target == nil || target.Resource().Alias() != alias {
		return false
	}
	return slices.ContainsFunc(target.Set(), func(assignment query.Assignment) bool { return assignment.Column() == name })
}

func queryWithWhere(q query.Unresolved, where []query.Predicate) (query.Unresolved, error) {
	if target := q.Target(); target != nil {
		return query.NewMutation(target, q.From(), where, q.Select())
	}
	return query.New(q.From(), where, q.Select())
}
