package omnisdk

import (
	"slices"
	"strings"
)

// starColumns uses the same operation selection as execution, after inbox
// producers are known. Alternative operation schemas are not one result shape.
func (r *resolver) starColumns(alias string) ([]string, error) {
	source, isDocument := r.tables[alias].(table)
	if !isDocument || source.catalog == nil || alias == r.mutating {
		return starColumnsOf(r.tables[alias]), nil
	}
	available := map[string]any{}
	for name, value := range r.params {
		available[name] = value
	}
	for name := range r.wide {
		available[name] = true
	}
	for name, value := range r.nodeParams[alias] {
		available[name] = value
	}
	for name := range r.fanout[alias] {
		available[name] = true
	}
	for name, value := range r.readBodies[alias] {
		available[name] = value
	}
	for _, input := range r.arrivals[alias] {
		if !strings.HasPrefix(input.as, "\x00") {
			available[input.as] = true
		}
	}
	exchange, err := chooseExchange(source.catalog, "", source.address, "select", available, nil, "")
	if err != nil {
		// Callers may supply additional bindings through Args when building
		// the executable graph. Preserve deferred resolution in that case.
		return starColumnsOf(source), nil
	}
	method := docSignature{ex: exchange}
	if r.selected == nil {
		r.selected = map[string]MethodSignature{}
	}
	r.selected[alias] = method
	columns := method.Columns()

	slices.Sort(columns)
	return columns, nil
}
