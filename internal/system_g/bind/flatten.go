package bind

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// tupleFlatten is the presentation transform that merges a bind-join (input, output) tuple
// into one flat row: input fields ∪ output fields (output wins on conflict). Left-outer rows
// (output == nil) pass through as input alone. This is the merge the join deliberately does
// NOT do — it lives here, on the egress path, as a T.
type tupleFlatten struct{}

// NewTupleFlatten builds the tuple-flattening transform.
func NewTupleFlatten() facade.Transform {
	return tupleFlatten{}
}

func (t tupleFlatten) Apply(in facade.Page) (facade.Record, error) {
	m, ok := DocMap(in)
	if !ok {
		return nil, nil
	}
	input, _ := m[KeyInput].(map[string]any)
	output, _ := m[KeyOutput].(map[string]any)
	if output == nil {
		return NewDocRecord(input), nil
	}
	return NewDocRecord(mergeMaps(input, output)), nil
}

// innerFlatten is tupleFlatten with INNER-join semantics: a left-outer row (output == nil) is
// DROPPED rather than passed through (a nil record is filtered downstream). Use it on intermediate
// descent stages — a scope with no children (a folder with no projects) then contributes nothing,
// so it never leaks an empty scope into a later visitor. Only the LEAF visitor keeps left-outer
// (tupleFlatten), so an empty leaf (a project with no buckets) is still reported.
type innerFlatten struct{}

// NewInnerFlatten builds the inner-join tuple flatten (drops unmatched rows).
func NewInnerFlatten() facade.Transform { return innerFlatten{} }

func (innerFlatten) Apply(in facade.Page) (facade.Record, error) {
	m, ok := DocMap(in)
	if !ok {
		return nil, nil
	}
	output, ok := m[KeyOutput].(map[string]any)
	if !ok || output == nil {
		return nil, nil // drop: no match at this intermediate stage
	}
	input, _ := m[KeyInput].(map[string]any)
	return NewDocRecord(mergeMaps(input, output)), nil
}
