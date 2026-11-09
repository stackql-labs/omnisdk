package starlark

import "go.starlark.net/syntax"

// expansion conservatively bounds logical value size, including shared aliases.
// This prevents tiny programs building exponentially expensive composite values
// for native equality/hash operations that VM instruction limits cannot interrupt.
func expansion(expr syntax.Expr, bindings map[string]int) int {
	sum := func(list []syntax.Expr) int {
		total := 8
		for _, item := range list {
			total += expansion(item, bindings)
			if total > 8*MaxValueBytes {
				return total
			}
		}
		return total
	}
	switch e := expr.(type) {
	case *syntax.Ident:
		if size, ok := bindings[e.Name]; ok {
			return size
		}
		return 8
	case *syntax.Literal:
		if text, ok := e.Value.(string); ok {
			return len(text) + 8
		}
		return 8
	case *syntax.ListExpr:
		return sum(e.List)
	case *syntax.TupleExpr:
		return sum(e.List)
	case *syntax.DictExpr:
		return sum(e.List)
	case *syntax.DictEntry:
		return 8 + expansion(e.Key, bindings) + expansion(e.Value, bindings)
	case *syntax.ParenExpr:
		return expansion(e.X, bindings)
	case *syntax.IndexExpr:
		return expansion(e.X, bindings)
	case *syntax.SliceExpr:
		return expansion(e.X, bindings)
	case *syntax.DotExpr:
		if e.Name.Name == "present" || e.Name.Name == "done" {
			return 8
		}
		return expansion(e.X, bindings)
	case *syntax.UnaryExpr:
		return expansion(e.X, bindings)
	case *syntax.BinaryExpr:
		if e.Op == syntax.AND || e.Op == syntax.OR {
			return max(expansion(e.X, bindings), expansion(e.Y, bindings))
		}
		return 8
	case *syntax.CondExpr:
		return max(expansion(e.True, bindings), expansion(e.False, bindings))
	case *syntax.CallExpr:
		if method, ok := e.Fn.(*syntax.DotExpr); ok && method.Name.Name == "get" {
			result := expansion(method.X, bindings)
			for _, arg := range e.Args {
				result = max(result, expansion(arg, bindings))
			}
			return result
		}
		return 8
	default:
		return 8
	}
}
