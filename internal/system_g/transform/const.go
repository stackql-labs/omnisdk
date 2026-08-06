package transform

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// constFields merges fixed key/values onto the agnostic row (e.g. tagging every row with its
// provider). An egress transform: it reads and returns the row, not a bind tuple.
type constFields struct{ fields map[string]any }

// NewConst adds constant fields to each row.
func NewConst(fields map[string]any) facade.Transform { return constFields{fields: fields} }

func (t constFields) Apply(in facade.Page) (facade.Record, error) {
	row := map[string]any{}
	if doc, ok := in.Doc(facade.AnonymousPayload); ok {
		if m, ok := doc.(map[string]any); ok {
			for k, v := range m {
				row[k] = v
			}
		}
	}
	for k, v := range t.fields {
		row[k] = v
	}
	return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(row)}), nil
}
