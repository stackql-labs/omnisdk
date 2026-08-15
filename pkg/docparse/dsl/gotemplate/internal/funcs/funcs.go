// Package funcs is the helper library the document's Go-template programs are written against.
//
// These are replicated deliberately, not imported: the documents are the contract, and a helper's
// behaviour is part of how a document reads. Keeping them here means the set is enumerable and
// testable rather than inherited. text/template's own builtins (index, printf, len, urlquery, eq,
// and, or, not, with, range, define, template, break) are NOT redefined — a document leaning on a
// builtin must get the builtin.
package funcs

import (
	"encoding/json"
	"fmt"
	"reflect"
	"text/template"
)

// Map is the helper set, ready for template.Funcs.
func Map() template.FuncMap {
	return template.FuncMap{
		"toJson":            ToJSON,
		"jsonMapFromString": JSONMapFromString,
		"plus1":             Plus1,
		"kindOf":            KindOf,
		// index OVERRIDES the builtin. Documents index optional response fields several levels deep
		// (index . "placement" "availabilityZone") on records that legitimately lack them; the builtin
		// errors on indexing nil, which would turn an absent optional field into a failed audit rather
		// than a null. Tolerating the miss is what makes the documents' own programs run.
		"index": Index,
	}
}

// Names are the helpers this package provides, for asserting coverage against a document.
func Names() []string {
	out := make([]string, 0, len(Map()))
	for k := range Map() {
		out = append(out, k)
	}
	return out
}

// ToJSON renders a value as JSON. Templates use it to emit a fragment verbatim, so HTML escaping
// would corrupt the output — encoding/json's default escaping of <, > and & is disabled.
func ToJSON(v any) (string, error) {
	var b []byte
	buf := &jsonBuffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("toJson: %w", err)
	}
	b = buf.trimNewline()
	return string(b), nil
}

// JSONMapFromString parses a JSON object from a string. An empty string is an empty map, because a
// document uses this on a request context that is legitimately absent.
func JSONMapFromString(s string) (map[string]any, error) {
	if s == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("jsonMapFromString: %w", err)
	}
	return m, nil
}

// Plus1 increments — documents use it to turn a 0-based range index into AWS's 1-based list
// parameters (Filter.1, Filter.2, …).
func Plus1(v any) (int, error) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(rv.Int()) + 1, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int(rv.Uint()) + 1, nil
	case reflect.Float32, reflect.Float64:
		return int(rv.Float()) + 1, nil
	default:
		return 0, fmt.Errorf("plus1: not a number: %T", v)
	}
}

// KindOf names a value's kind, which is how a document branches on whether a decoded field came back
// as a single object or a list — the central ambiguity of XML-to-map decoding, where a one-element
// list is indistinguishable from a scalar.
func KindOf(v any) string {
	if v == nil {
		return "invalid"
	}
	return reflect.ValueOf(v).Kind().String()
}

// Index walks keys into nested maps and slices, returning nil at the first miss instead of erroring.
// A miss is an answer — the field is absent — so it must be expressible as null rather than fatal.
func Index(item any, keys ...any) any {
	cur := item
	for _, k := range keys {
		if cur == nil {
			return nil
		}
		rv := reflect.ValueOf(cur)
		switch rv.Kind() {
		case reflect.Map:
			kv := reflect.ValueOf(k)
			if !kv.IsValid() || !kv.Type().AssignableTo(rv.Type().Key()) {
				return nil
			}
			got := rv.MapIndex(kv)
			if !got.IsValid() {
				return nil
			}
			cur = got.Interface()
		case reflect.Slice, reflect.Array:
			i, err := Plus1(k)
			if err != nil {
				return nil
			}
			i-- // Plus1 is the only numeric coercion here; undo its increment
			if i < 0 || i >= rv.Len() {
				return nil
			}
			cur = rv.Index(i).Interface()
		default:
			return nil
		}
	}
	return cur
}

// jsonBuffer is a tiny sink so ToJSON can use an Encoder (for SetEscapeHTML) without importing bytes
// semantics into the helper's signature.
type jsonBuffer struct{ b []byte }

func (w *jsonBuffer) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

// trimNewline drops the newline Encoder.Encode appends.
func (w *jsonBuffer) trimNewline() []byte {
	if n := len(w.b); n > 0 && w.b[n-1] == '\n' {
		return w.b[:n-1]
	}
	return w.b
}
