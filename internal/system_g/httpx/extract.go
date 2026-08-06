package httpx

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// NewExtract is the canonical response flatten: over a bind-join tuple {input, output}, it merges
// onto the input row (a) literal fields and (b) fields pulled by dotted path from the decoded
// output body. An output key may itself be dotted to build a nested object; a path that resolves
// empty is omitted (optional fields). decode is the swappable body decoder — nil = JSON,
// transform.NewXMLToAgnostic for XML — so JSON and XML extraction differ only in that argument.
// No status logic lives here; branching on the response code is NewStatusBranch.
func NewExtract(decode facade.Transform, fields map[string]string, literals map[string]any) facade.Transform {
	return extract{decode: decode, fields: fields, literals: literals}
}

type extract struct {
	decode   facade.Transform
	fields   map[string]string
	literals map[string]any
}

func (t extract) Apply(in facade.Page) (facade.Record, error) {
	m, ok := bind.DocMap(in)
	if !ok {
		return nil, nil
	}
	input, _ := m[bind.KeyInput].(map[string]any)
	row := maps.Clone(input)
	if row == nil {
		row = map[string]any{}
	}
	for k, v := range t.literals {
		setPath(row, k, v)
	}
	output, _ := m[bind.KeyOutput].(map[string]any)
	if output == nil {
		return bind.NewDocRecord(row), nil // left-outer: no response side
	}
	if len(t.fields) > 0 {
		doc, err := decodeDoc(t.decode, []byte(str(output[bind.KeyRaw])))
		if err != nil {
			return nil, err
		}
		for out, path := range t.fields {
			if v := DocPath(doc, path); v != "" {
				setPath(row, out, v)
			}
		}
	}
	return bind.NewDocRecord(row), nil
}

// NewStatusBranch is the canonical behavioural edge (E_α) on an HTTP response's status: it reads
// the status of a bind-join tuple's output and dispatches to the matching case flatten. An
// unmatched status runs def; if def is nil, an unmatched status fails the flow. A left-outer
// tuple (no response) passes the input row through.
func NewStatusBranch(cases map[string]facade.Transform, def facade.Transform) facade.Transform {
	return statusBranch{cases: cases, def: def}
}

type statusBranch struct {
	cases map[string]facade.Transform
	def   facade.Transform
}

func (b statusBranch) Apply(in facade.Page) (facade.Record, error) {
	m, ok := bind.DocMap(in)
	if !ok {
		return nil, nil
	}
	output, _ := m[bind.KeyOutput].(map[string]any)
	if output == nil {
		input, _ := m[bind.KeyInput].(map[string]any)
		return bind.NewDocRecord(input), nil // left-outer
	}
	status := str(output["status"])
	if tr, ok := b.cases[status]; ok {
		return tr.Apply(in)
	}
	if b.def != nil {
		return b.def.Apply(in)
	}
	return nil, fmt.Errorf("http: status %s: %s", status, str(output[bind.KeyRaw]))
}

// decodeDoc turns a raw body into an agnostic document via the swappable decoder (nil = JSON).
func decodeDoc(decode facade.Transform, raw []byte) (any, error) {
	if decode == nil {
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		return doc, nil
	}
	rec, err := decode.Apply(record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewBytesValue(raw),
	}))
	if err != nil || rec == nil {
		return nil, err
	}
	doc, _ := rec.Doc(facade.AnonymousPayload)
	return doc, nil
}

// setPath sets row[a][b]... = v for a dotted key, creating intermediate maps.
func setPath(row map[string]any, key string, v any) {
	parts := strings.Split(key, ".")
	m := row
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}
