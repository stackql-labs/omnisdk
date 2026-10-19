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
func pushdown(ex aot.AOTExchange, g Graph, alias string, needed []string, limit int) map[string]string {
	pd, ok := ex.Pushdown()
	if !ok {
		return nil
	}
	out := map[string]string{}
	filters, all := ownFilters(g, alias)
	var filtered []string
	if f, ok := pd.Filter(); ok && f.Dialect() == "odata" && f.ParamName() != "" {
		var terms []string
		pushed := 0
		for _, c := range filters {
			col, op, lit, ok := comparison(c)
			if !ok || !f.SupportsColumn(col) || !f.SupportsOperator(op) {
				continue
			}
			if len(terms) == 1 && !f.SupportsOperator("and") {
				continue
			}
			terms = append(terms, fmt.Sprintf("%s %s %s", col, op, odataLiteral(lit)))
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
		if cols, ok := projected(g, alias); ok {
			// The server is asked for exactly what is read: the select list, and whatever else
			// reads this node. A column the option cannot name means the projection is not sent at
			// all — a partial one would drop a column something still needs.
			want := append(append(cols, needed...), filtered...)
			// A private name is the query's own — a kept or a computed column — made from the
			// row's fields, which the select list already names.
			want = slices.DeleteFunc(want, func(c string) bool { return strings.HasPrefix(c, "\x00") })
			slices.Sort(want)
			want = slices.Compact(want)
			if !slices.ContainsFunc(want, func(c string) bool { return !s.SupportsColumn(c) }) {
				out[s.ParamName()] = strings.Join(want, s.Delimiter())
			}
		}
	}
	// A cap on rows is only the run's limit where nothing after the server could drop a row: this is
	// the graph's only read, and every filter on it was sent.
	if t, ok := pd.Top(); ok && t.ParamName() != "" && limit > 0 && all && reads(g) == 1 {
		v := limit
		if m := t.MaxValue(); m > 0 && v > m {
			v = m
		}
		out[t.ParamName()] = strconv.Itoa(v)
	}
	return out
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
