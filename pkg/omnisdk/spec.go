package omnisdk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// LoadResources reads the desired state in the file at path: the resources to converge, with no
// collection — which collection they converge under is the caller's to name, so one file can be
// applied as many collections.
func LoadResources(path string) ([]ManagedResource, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("omnisdk: resources file: %w", err)
	}
	rs, err := ParseResources(b)
	if err != nil {
		return nil, fmt.Errorf("omnisdk: %s: %w", path, err)
	}
	return rs, nil
}

// ParseResources reads desired state written as JSON: a list of resources, or an object whose
// "resources" is that list — so an omnicli iac-apply spec is a resources file too, its collection
// and state ignored. Each field may be written snake_case or camelCase, addressed_by or
// addressedBy, as either caller already writes it.
func ParseResources(b []byte) ([]ManagedResource, error) {
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(b, &list); err != nil {
		var wrapped struct {
			Resources []map[string]json.RawMessage `json:"resources"`
		}
		if werr := json.Unmarshal(b, &wrapped); werr != nil || wrapped.Resources == nil {
			return nil, fmt.Errorf("not a list of resources, nor an object with one: %w", err)
		}
		list = wrapped.Resources
	}
	out := make([]ManagedResource, 0, len(list))
	for i, m := range list {
		r, err := resourceFrom(m)
		if err != nil {
			return nil, fmt.Errorf("resource %d: %w", i, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func resourceFrom(m map[string]json.RawMessage) (ManagedResource, error) {
	str := func(names ...string) (string, error) {
		for _, n := range names {
			if raw, ok := m[n]; ok {
				var s string
				if err := json.Unmarshal(raw, &s); err != nil {
					return "", fmt.Errorf("%s is not a string", n)
				}
				return s, nil
			}
		}
		return "", nil
	}
	if _, old := m["correlation_param"]; old {
		return nil, fmt.Errorf("correlation_param was removed; use debug_correlation_param with target and transformer")
	}
	if _, old := m["correlationParam"]; old {
		return nil, fmt.Errorf("correlationParam was removed; use debug_correlation_param with target and transformer")
	}
	var debug DebugCorrelationParam
	if raw, ok := m["debug_correlation_param"]; ok {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&debug); err != nil {
			return nil, fmt.Errorf("debug_correlation_param: %w", err)
		}
	}
	if err := validateDebugCorrelation(debug); err != nil {
		return nil, err
	}
	var fields [7]string
	for i, names := range [][]string{
		{"key"}, {"provider"}, {"address"}, {"identity"},
		{"addressed_by", "addressedBy"},
		{"via_type", "viaType"}, {"via", "via_program", "viaProgram"},
	} {
		v, err := str(names...)
		if err != nil {
			return nil, err
		}
		fields[i] = v
	}
	if fields[0] == "" {
		return nil, fmt.Errorf("no key")
	}
	desired := []byte(m["desired"])
	if len(desired) == 0 {
		desired = []byte(`{}`)
	}
	var params map[string]string
	if raw, ok := m["params"]; ok {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, fmt.Errorf("%s: params: %w", fields[0], err)
		}
	}
	var inbound []Arrival
	if raw, ok := m["inbound"]; ok {
		var in []struct {
			From string `json:"from"`
			As   string `json:"as"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("%s: inbound: %w", fields[0], err)
		}
		for _, a := range in {
			inbound = append(inbound, Arrival{From: a.From, As: a.As})
		}
	}
	return WithDebugCorrelationParam(NewResource(fields[0], fields[1], fields[2], desired, params, inbound, fields[5], fields[6],
		fields[3], fields[4], ""), debug), nil
}
