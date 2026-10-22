package omnisdk

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/fn"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
	"github.com/stackql-labs/omnisdk/pkg/query"
)

// Server-side projection and filtering: what a read's method can be asked to do so less comes back,
// as its document's queryParamPushdown declares it. It only ever narrows what the server sends. Every
// filter still runs on the rows, so a server that matches more loosely — case-insensitively, say —
// cannot change the answer, and a filter the server cannot take is simply not sent.

// pushdown is the query parameters node alias's read can carry, by wire name. needed are the columns
// anything other than its select list reads from it: an edge, a filter, a join condition.
func pushdown(ex aot.AOTExchange, g Graph, alias string, needed []string, limit int, hint ReadHints) (map[string]string, int) {
	pd, ok := ex.Pushdown()
	if !ok {
		return nil, 0
	}
	out := map[string]string{}
	filters, all := ownFilters(g, alias)
	var filtered []string
	if f, ok := pd.Filter(); ok && f.Dialect() == "odata" && f.ParamName() != "" {
		var terms []string
		pushed := 0
		for _, c := range filters {
			col, term, ok := odataTerm(c, f)
			if !ok {
				continue
			}
			if len(terms) == 1 && !f.SupportsOperator("and") {
				continue
			}
			terms = append(terms, term)
			filtered = append(filtered, col)
			pushed++
		}
		if len(terms) > 0 {
			out[f.ParamName()] = strings.Join(terms, " and ")
		}
		all = all && pushed == len(filters)
	} else {
		all = all && len(filters) == 0
	}
	if s, ok := pd.Select(); ok && s.ParamName() != "" {
		if cols, ok := projected(g, alias); ok && !hint.AllColumns {
			// Hints order the fields actually consumed by the plan; request
			// bindings mentioned in WHERE are not necessarily response columns.
			orderedColumns := []string{}
			for _, name := range hint.Columns {
				if slices.Contains(cols, name) || slices.Contains(needed, name) || slices.Contains(filtered, name) {
					orderedColumns = append(orderedColumns, name)
				}
			}
			cols = append(orderedColumns, cols...)
			// The server is asked for exactly what is read: the select list, and whatever else
			// reads this node. A column the option cannot name means the projection is not sent at
			// all — a partial one would drop a column something still needs.
			want := append(append(cols, needed...), filtered...)
			// A private name is the query's own — a kept or a computed column — made from the
			// row's fields, which the select list already names.
			want = slices.DeleteFunc(want, func(c string) bool { return strings.HasPrefix(c, "\x00") })
			if len(hint.Columns) == 0 {
				slices.Sort(want)
				want = slices.Compact(want)
			} else {
				seen := map[string]bool{}
				unique := want[:0]
				for _, name := range want {
					if !seen[name] {
						unique = append(unique, name)
						seen[name] = true
					}
				}
				want = unique
			}
			if !slices.ContainsFunc(want, func(c string) bool { return !s.SupportsColumn(c) }) {
				out[s.ParamName()] = strings.Join(want, s.Delimiter())
			}
		}
	}
	appliedOffset := 0
	ordered := len(hint.OrderBy) == 0
	if controls, ok := pd.(aot.ReadControls); ok {
		if count, ok := controls.Count(); ok && hint.Count && count.ParamName() != "" {
			out[count.ParamName()] = "true"
		}
		if order, ok := controls.OrderBy(); ok && !hint.DisableRowControls && order.ParamName() != "" && len(hint.OrderBy) > 0 {
			if rendered, ok := renderOrder(order, hint.OrderBy); ok {
				out[order.ParamName()] = rendered
				ordered = true
				if syntax, ok := order.(aot.OrderSyntax); ok && syntax.Syntax() == "column_only" {
					ordered = false
				}
			}
		}
		// OFFSET cannot precede local row rejection, joins, or an unpushed sort.
		if skip, ok := controls.Skip(); ok && !hint.DisableRowControls && hint.Offset > 0 && len(filters) == 0 && all && len(g.Nodes()) == 1 && ordered && skip.ParamName() != "" {
			out[skip.ParamName()] = strconv.Itoa(hint.Offset)
			appliedOffset = hint.Offset
		}
	}
	if hint.Limit > 0 && !hint.DisableRowControls {
		limit = hint.Limit
	}
	if hint.DisableRowControls {
		limit = 0
	}
	if appliedOffset == 0 && hint.Offset > 0 && limit > 0 {
		if hint.Offset > int(^uint(0)>>1)-limit {
			limit = 0
		} else {
			limit += hint.Offset
		}
	}
	// A cap on rows is only the run's limit where nothing after the server could drop a row: this is
	// the graph's only read, and every filter on it was sent.
	if t, ok := pd.Top(); ok && t.ParamName() != "" && limit > 0 && all && ordered && reads(g) == 1 {
		v := limit
		if m := t.MaxValue(); m > 0 && v > m {
			v = m
		}
		out[t.ParamName()] = strconv.Itoa(v)
	}
	return out, appliedOffset
}

// ownFilters are g's filters that read alias alone. all is false where some filter reads alias
// together with another node, which the server cannot evaluate.
func ownFilters(g Graph, alias string) ([]query.Predicate, bool) {
	var own []query.Predicate
	all := true
	for _, f := range g.Filters() {
		cols := predicateColumns(f)
		mine := slices.ContainsFunc(cols, func(c query.Column) bool { return c.Qualifier() == alias })
		if !mine {
			continue
		}
		if slices.ContainsFunc(cols, func(c query.Column) bool { return c.Qualifier() != alias }) {
			all = false
			continue
		}
		own = append(own, f)
	}
	return own, all
}

// comparison reads p as column op literal, with op in OData's names, turning literal op column
// around.
func comparison(p query.Predicate) (string, string, any, bool) {
	c, ok := p.(query.Compare)
	if !ok {
		return "", "", nil, false
	}
	op, ok := map[query.CompareOp]string{query.Eq: "eq", query.Ne: "ne", query.Lt: "lt", query.Le: "le", query.Gt: "gt", query.Ge: "ge"}[c.Op()]
	if !ok {
		return "", "", nil, false
	}
	if col, isCol := c.Left().(query.Column); isCol {
		if lit, isLit := c.Right().(query.Literal); isLit {
			name, ok := serverColumn(col)
			return name, op, lit.Value(), ok
		}
	}
	if col, isCol := c.Right().(query.Column); isCol {
		if lit, isLit := c.Left().(query.Literal); isLit {
			flip := map[string]string{"lt": "gt", "le": "ge", "gt": "lt", "ge": "le", "eq": "eq", "ne": "ne"}
			name, ok := serverColumn(col)
			return name, flip[op], lit.Value(), ok
		}
	}
	return "", "", nil, false
}

// serverColumn is the field a filter's column names on the server. The resolver keeps a filtered
// column under a private name, the field's own behind a NUL; another node's value, under its node's
// private key, is nothing the server holds.
func serverColumn(c query.Column) (string, bool) {
	name := strings.TrimPrefix(c.Name(), "\x00")
	return name, !strings.Contains(name, "\x00") && name != ""
}

// odataLiteral writes v as OData does: a string quoted with quotes doubled, a boolean or a number
// bare.
func odataLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

// projected are the row columns alias's select list reads, false where it has none — every column
// is wanted then.
func projected(g Graph, alias string) ([]string, bool) {
	p, ok := projectionFor(g, alias)
	if !ok {
		return nil, false
	}
	var out []string
	for _, c := range p.internal() {
		out = append(out, fn.Fields(c.Expr)...)
	}
	return out, len(out) > 0
}

// reads counts g's nodes that call something: everything but the intrinsics.
func reads(g Graph) int {
	n := 0
	for _, x := range g.Nodes() {
		if _, intrinsic := x.(intrinsicNode); !intrinsic {
			n++
		}
	}
	return n
}

func odataTerm(p query.Predicate, f aot.PushdownOption) (string, string, bool) {
	col, op, lit, ok := comparison(p)
	if ok && f.SupportsColumn(col) && f.SupportsOperator(op) {
		return col, fmt.Sprintf("%s %s %s", col, op, odataLiteral(lit)), true
	}
	test, ok := p.(query.Test)
	if !ok {
		return "", "", false
	}
	call, ok := test.Cond().(query.Call)
	if !ok || !strings.EqualFold(call.Func(), "like") || len(call.Args()) != 2 {
		return "", "", false
	}
	var column query.Column
	var pattern string
	for _, arg := range call.Args() {
		if c, ok := arg.(query.Column); ok {
			column = c
		}
		if l, ok := arg.(query.Literal); ok {
			pattern, _ = l.Value().(string)
		}
	}
	if column == nil || strings.ContainsAny(pattern, "_\\") {
		return "", "", false
	}
	name, ok := serverColumn(column)
	if !ok || !f.SupportsColumn(name) {
		return "", "", false
	}
	prefix, suffix := strings.HasPrefix(pattern, "%"), strings.HasSuffix(pattern, "%")
	text := strings.TrimSuffix(strings.TrimPrefix(pattern, "%"), "%")
	if strings.Contains(text, "%") {
		return "", "", false
	}
	function := "eq"
	switch {
	case prefix && suffix:
		function = "contains"
	case prefix:
		function = "endswith"
	case suffix:
		function = "startswith"
	}
	if !f.SupportsOperator(function) {
		return "", "", false
	}
	if function == "eq" {
		return name, name + " eq " + odataLiteral(text), true
	}
	return name, fmt.Sprintf("%s(%s,%s)", function, name, odataLiteral(text)), true
}

func renderOrder(option aot.PushdownOption, terms []ReadOrder) (string, bool) {
	syntax := "odata"
	fixed := ""
	if style, ok := option.(aot.OrderSyntax); ok {
		syntax = style.Syntax()
		fixed = style.FixedColumn()
	}
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		if !option.SupportsColumn(term.Column) {
			return "", false
		}
		direction := "asc"
		if term.Descending {
			direction = "desc"
		}
		switch syntax {
		case "direction_only":
			if len(terms) != 1 || fixed != term.Column {
				return "", false
			}
			out = append(out, direction)
		case "column_only":
			out = append(out, term.Column)
		case "prefix":
			prefix := ""
			if term.Descending {
				prefix = "-"
			}
			out = append(out, prefix+term.Column)
		case "suffix":
			out = append(out, term.Column+":"+direction)
		case "odata", "":
			out = append(out, term.Column+" "+direction)
		default:
			return "", false
		}
	}
	return strings.Join(out, option.Delimiter()), len(out) > 0
}
