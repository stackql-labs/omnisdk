package starlark

import (
	"fmt"
	"regexp"

	star "go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

const MaxSourceBytes = 16384
const MaxValueBytes = 1048576

// Validate checks the bounded v1 subset before a plan can execute effects.
// General loops, user functions, dynamic calls and allocation-amplifying
// arithmetic are deliberately outside this initial admission/transform language.
func Validate(program string) error {
	if len(program) > MaxSourceBytes {
		return fmt.Errorf("starlark source exceeds %d bytes", MaxSourceBytes)
	}
	var root syntax.Node
	expression, err := syntax.ParseExpr("program.star", program, 0)
	if err == nil {
		root = expression
	} else {
		file, parseErr := syntax.Parse("program.star", program, 0)
		if parseErr != nil {
			return parseErr
		}
		root = file
		expression = nil
	}
	err = nil
	count := 0
	regexCalls := 0
	sizes := map[string]int{"inbox": MaxValueBytes, "edges": MaxValueBytes}
	syntax.Walk(root, func(node syntax.Node) bool {
		if node == nil || err != nil {
			return false
		}
		count++
		if count > 1024 {
			err = fmt.Errorf("starlark program exceeds 1024 syntax nodes")
			return false
		}
		if expression, ok := node.(syntax.Expr); ok && expansion(expression, sizes) > 8*MaxValueBytes {
			err = fmt.Errorf("Starlark composite expansion exceeds static resource limit")
			return false
		}
		switch n := node.(type) {
		case *syntax.ForStmt, *syntax.WhileStmt, *syntax.Comprehension, *syntax.DefStmt, *syntax.LambdaExpr, *syntax.LoadStmt:
			err = fmt.Errorf("%T is outside the bounded Starlark subset", node)
		case *syntax.BinaryExpr:
			switch n.Op {
			case syntax.AND, syntax.OR, syntax.EQL, syntax.NEQ, syntax.LT, syntax.LE, syntax.GT, syntax.GE, syntax.IN, syntax.NOT_IN:
			default:
				err = fmt.Errorf("operator %s is outside the bounded Starlark subset", n.Op)
			}
		case *syntax.UnaryExpr:
			if n.Op != syntax.NOT && n.Op != syntax.MINUS && n.Op != syntax.PLUS {
				err = fmt.Errorf("operator %s is not supported", n.Op)
			}
		case *syntax.AssignStmt:
			if n.Op != syntax.EQ {
				err = fmt.Errorf("augmented assignment is not supported")
			}
			if ident, ok := n.LHS.(*syntax.Ident); !ok {
				err = fmt.Errorf("only local variable assignment is supported")
			} else if ident.Name == "inbox" || ident.Name == "edges" || ident.Name == "re" || star.Universe.Has(ident.Name) {
				err = fmt.Errorf("cannot rebind protected name %s", ident.Name)
			} else {
				sizes[ident.Name] = max(sizes[ident.Name], expansion(n.RHS, sizes))
				if sizes[ident.Name] > 8*MaxValueBytes {
					err = fmt.Errorf("Starlark composite expansion exceeds static resource limit")
				}
			}
		case *syntax.CallExpr:
			if ident, ok := n.Fn.(*syntax.Ident); ok {
				switch ident.Name {
				case "len", "bool", "all", "any":
				default:
					err = fmt.Errorf("call to %s is not supported", ident.Name)
				}
			} else if dot, ok := n.Fn.(*syntax.DotExpr); ok {
				if ident, ok := dot.X.(*syntax.Ident); ok && ident.Name == "re" && (dot.Name.Name == "search" || dot.Name.Name == "fullmatch") {
					regexCalls++
					if regexCalls > 8 {
						err = fmt.Errorf("at most 8 regexp calls are permitted")
						break
					}
					if len(n.Args) != 2 {
						err = fmt.Errorf("regexp requires two positional arguments")
						break
					}
					literal, ok := n.Args[0].(*syntax.Literal)
					if !ok {
						err = fmt.Errorf("regexp pattern must be a literal")
						break
					}
					pattern, ok := literal.Value.(string)
					if !ok || len(pattern) > 1024 {
						err = fmt.Errorf("regexp pattern must be a string of at most 1024 bytes")
						break
					}
					_, err = regexp.Compile(pattern)
				} else if dot.Name.Name != "get" {
					err = fmt.Errorf("method %s is not supported", dot.Name.Name)
				}
			} else {
				err = fmt.Errorf("dynamic calls are not supported")
			}
		}
		return err == nil
	})
	if err != nil {
		return err
	}
	// Resolve names and Starlark semantic restrictions without executing the program.
	predefined := func(name string) bool {
		return name == "inbox" || name == "edges" || name == "re" || star.Universe.Has(name)
	}
	if expression != nil {
		_, err = star.ExprFunc("program.star", program, star.StringDict{"inbox": star.None, "edges": star.None, "re": star.None})
	} else {
		_, _, err = star.SourceProgram("program.star", program, predefined)
	}
	return err
}

func (evaluator) Validate(program string) error { return Validate(program) }
