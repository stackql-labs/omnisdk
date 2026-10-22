package docx

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

// AWSCanonical names the query transposition AWS's Query protocol uses, as documents declare it.
const AWSCanonical = "AWSCanonical"

// transposed is req for one row under AWSCanonical: a query parameter bound to a list or a map is
// written as the members AWS's Query protocol takes — a list's as name.1, name.2, a map's as
// name.Key, each nested in turn — rather than as one value. A scalar is sent as it is.
//
// The members are bound under names of their own, so a value is placed exactly as written and never
// read as a template.
func transposed(req httpx.Request, bound map[string]any) (httpx.Request, map[string]any) {
	var out httpx.Request
	var in map[string]any
	n := 0
	for wire, tmpl := range req.Query {
		name, ok := strings.CutPrefix(tmpl, "{")
		if name, ok = strings.CutSuffix(name, "}"); !ok || strings.ContainsAny(name, "{}") {
			continue
		}
		v := bound[name]
		if text, ok := v.(string); ok {
			trimmed := strings.TrimSpace(text)
			if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{') && json.Valid([]byte(trimmed)) {
				decoder := json.NewDecoder(strings.NewReader(trimmed))
				decoder.UseNumber()
				var structured any
				if decoder.Decode(&structured) == nil {
					v = structured
				}
			}
		}
		switch v.(type) {
		case []any, map[string]any:
		default:
			continue
		}
		if in == nil {
			out, in = req, maps.Clone(bound)
			out.Query = maps.Clone(req.Query)
		}
		delete(out.Query, wire)
		for key, member := range canonical(name, v) {
			slot := fmt.Sprintf("__transposed_%d", n)
			n++
			in[slot] = member
			out.Query[key] = "{" + slot + "}"
		}
	}
	if in == nil {
		return req, bound
	}
	return out, in
}

// canonical flattens v under base the way AWS's Query protocol writes structured values.
func canonical(base string, v any) map[string]string {
	out := map[string]string{}
	switch x := v.(type) {
	case []any:
		for i, e := range x {
			maps.Copy(out, canonical(fmt.Sprintf("%s.%d", base, i+1), e))
		}
	case map[string]any:
		for k, e := range x {
			maps.Copy(out, canonical(base+"."+k, e))
		}
	default:
		out[base] = fmt.Sprint(x)
	}
	return out
}
