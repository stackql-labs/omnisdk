package bind

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// selectKeys is a projection over the agnostic row: it keeps only the named keys (dropping the
// rest — e.g. an auth token that must not reach the output). Absent keys are simply omitted.
type selectKeys struct {
	keys []string
}

// NewSelect builds a transform that projects the row down to keys.
func NewSelect(keys []string) facade.Transform {
	return selectKeys{keys: keys}
}

func (t selectKeys) Apply(in facade.Page) (facade.Record, error) {
	m, ok := DocMap(in)
	if !ok {
		return nil, nil
	}
	out := make(map[string]any, len(t.keys))
	for _, k := range t.keys {
		out[k] = m[k] // present value, or nil (→ JSON null) if absent, so every row has every key
	}
	return NewDocRecord(out), nil
}
