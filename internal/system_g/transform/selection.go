package transform

import (
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/fn"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

var _ facade.Transform = selection{}

// selection applies a select list to one row. It always emits a row LIST, so a row-producing
// function and a scalar one leave the same shape behind and one explode downstream serves both.
//
// At most one column may be row-producing: two would be a cross product, which is a join and is
// stated as one, not smuggled into a select list.
type selection struct {
	cols   []fn.Column
	reg    facade.FnRegistry
	fanout int // index of the row-producing column, -1 when the select is all scalar
}

// NewSelection builds a select-list transform. The row-producing column, if any, is found here
// rather than per row: the shape of the output is a property of the select list, and a select whose
// shape depended on the data would be unusable as a schema.
func NewSelection(cols []fn.Column, reg facade.FnRegistry) (facade.Transform, error) {
	fanout := -1
	for i, c := range cols {
		name := c.Expr.Fn()
		if name == "" {
			continue
		}
		rows, err := fn.RowProducing(reg, name)
		if err != nil {
			return nil, err
		}
		if !rows {
			continue
		}
		if fanout >= 0 {
			return nil, fmt.Errorf("transform: select has two row-producing columns (%s, %s)",
				cols[fanout].Out, c.Out)
		}
		fanout = i
	}
	return selection{cols: cols, reg: reg, fanout: fanout}, nil
}

func (t selection) Apply(in facade.Page) (facade.Record, error) {
	doc, ok := in.Doc(facade.AnonymousPayload)
	if !ok {
		return nil, fmt.Errorf("transform: selection input is not an agnostic document")
	}
	row, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("transform: selection input is not a row")
	}

	base := make(map[string]any, len(t.cols))
	var produced []any
	for i, c := range t.cols {
		v, err := c.Expr.Eval(row, t.reg)
		if err != nil {
			return nil, err
		}
		if i != t.fanout {
			base[c.Out] = v
			continue
		}
		rows, ok := v.([]any)
		if !ok {
			// NULL in, no rows out: the function propagated a NULL argument.
			continue
		}
		produced = rows
	}

	if t.fanout < 0 {
		return rowList([]any{base}), nil
	}
	out := make([]any, 0, len(produced))
	for _, p := range produced {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, merge(base, m, t.cols[t.fanout].Out))
	}
	return rowList(out), nil
}

// merge copies base and adds the produced row's fields. A single-column function is renamed to the
// select's own output name — that is what the caller asked the column to be called. A function
// emitting several columns keeps its own names, since one name cannot stand for several.
func merge(base, produced map[string]any, out string) map[string]any {
	row := make(map[string]any, len(base)+len(produced))
	for k, v := range base {
		row[k] = v
	}
	if len(produced) == 1 {
		for _, v := range produced {
			row[out] = v
		}
		return row
	}
	for k, v := range produced {
		row[k] = v
	}
	return row
}

func rowList(rows []any) facade.Record {
	return record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewDocValue(rows),
	})
}
