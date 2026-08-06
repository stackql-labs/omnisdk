// Package bind provides the β bind-join machinery (paper §β, §W): a Binding (identity
// value-transfer between exchange slots) and the driver-side dependent nested-loop join that
// realises it. Generic over the agnostic-map record form; no provider specifics.
package bind

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// DocMap returns the agnostic map carried in a page's anonymous payload — the A^out store this
// stage reads/writes. ok is false if the payload is not an agnostic map.
func DocMap(p facade.Page) (map[string]any, bool) {
	if p == nil {
		return nil, false
	}
	doc, ok := p.Doc(facade.AnonymousPayload)
	if !ok {
		return nil, false
	}
	m, ok := doc.(map[string]any)
	return m, ok
}

// NewDocRecord wraps an agnostic map as a record.
func NewDocRecord(m map[string]any) facade.Record {
	return record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewDocValue(m),
	})
}
