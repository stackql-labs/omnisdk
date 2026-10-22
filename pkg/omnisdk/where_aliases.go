package omnisdk

import "github.com/stackql-labs/omnisdk/pkg/query"

// SQLite permits SELECT aliases in WHERE when no source column has that name.
// Expand only that case; source columns and request inputs retain precedence.
func (r *resolver) whereAliases(p query.Predicate, outputs []query.Output) query.Predicate {
	var expr func(query.Expr) query.Expr
	expr = func(e query.Expr) query.Expr {
		switch e := e.(type) {
		case query.Column:
			if e.Qualifier() == "" && len(r.owners(e.Name())) == 0 {
				for _, output := range outputs {
					if output.Name() == e.Name() {
						return output.Expr()
					}
				}
			}
		case query.Call:
			args := make([]query.Expr, 0, len(e.Args()))
			for _, arg := range e.Args() {
				args = append(args, expr(arg))
			}
			return query.NewCall(e.Func(), args...)
		case query.Collection:
			items := make([]query.Expr, 0, len(e.Items()))
			for _, item := range e.Items() {
				items = append(items, expr(item))
			}
			return query.NewCollection(items...)
		}
		return e
	}
	var predicate func(query.Predicate) query.Predicate
	predicate = func(p query.Predicate) query.Predicate {
		switch p := p.(type) {
		case query.Compare:
			return query.NewCompare(p.Op(), expr(p.Left()), expr(p.Right()))
		case query.Test:
			return query.NewTest(expr(p.Cond()))
		case query.In:
			return query.NewIn(expr(p.Expr()), expr(p.Set()))
		case query.Not:
			return query.NewNot(predicate(p.Negated()))
		case query.Or:
			any := make([]query.Predicate, 0, len(p.Any()))
			for _, term := range p.Any() {
				any = append(any, predicate(term))
			}
			return query.NewOr(any...)
		}
		return p
	}
	return predicate(p)
}
