package resourcebindings

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type resolver struct{}

func New() facade.ResourceBindingResolver { return resolver{} }

func reference(value map[string]any) (string, string, bool, error) {
	from, hasFrom := value["from"]
	attr, hasAttr := value["attribute"]
	if !hasFrom || !hasAttr {
		if from == "self" || from == "env" || from == "input" {
			return "", "", true, fmt.Errorf("binding requires attribute")
		}
		return "", "", false, nil
	}
	source, ok := from.(string)
	if !ok {
		return "", "", true, fmt.Errorf("binding from must be a string")
	}
	name, ok := attr.(string)
	if !ok || name == "" {
		return "", "", true, fmt.Errorf("binding attribute must be a nonempty string")
	}
	if len(value) != 2 {
		return "", "", true, fmt.Errorf("binding accepts only from and attribute")
	}
	if source != "self" && source != "env" && source != "input" {
		return "", "", true, fmt.Errorf("unknown binding source %q", source)
	}
	return source, name, true, nil
}
func lookup(root map[string]any, path string) (any, bool) {
	if value, ok := root[path]; ok {
		return value, true
	}
	first, rest, nested := strings.Cut(path, ".")
	if !nested {
		return nil, false
	}
	child, ok := root[first].(map[string]any)
	if !ok {
		return nil, false
	}
	return lookup(child, rest)
}
func clone(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, v := range value {
			out[k] = clone(v)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, v := range value {
			out[i] = clone(v)
		}
		return out
	default:
		return value
	}
}
func (resolver) Resolve(value any, sources map[string]map[string]any) (any, error) {
	active := map[string]bool{}
	var walk func(any, int) (any, error)
	walk = func(value any, depth int) (any, error) {
		if depth > 256 {
			return nil, fmt.Errorf("binding nesting exceeds 256 levels")
		}
		switch value := value.(type) {
		case map[string]any:
			source, name, isRef, err := reference(value)
			if err != nil {
				return nil, err
			}
			if isRef {
				found, ok := lookup(sources[source], name)
				if !ok {
					return nil, fmt.Errorf("unresolved %s binding %q", source, name)
				}
				if source != "self" {
					return clone(found), nil
				}
				if active[name] {
					return nil, fmt.Errorf("cyclic self binding %q", name)
				}
				active[name] = true
				out, err := walk(found, depth+1)
				delete(active, name)
				return out, err
			}
			out := make(map[string]any, len(value))
			for _, k := range slices.Sorted(maps.Keys(value)) {
				v, err := walk(value[k], depth+1)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", k, err)
				}
				out[k] = v
			}
			return out, nil
		case []any:
			out := make([]any, len(value))
			for i, v := range value {
				resolved, err := walk(v, depth+1)
				if err != nil {
					return nil, fmt.Errorf("[%d]: %w", i, err)
				}
				out[i] = resolved
			}
			return out, nil
		default:
			return value, nil
		}
	}
	return walk(value, 0)
}
func (resolver) References(value any, source string) ([]string, error) {
	names := map[string]bool{}
	var walk func(any) error
	walk = func(value any) error {
		switch value := value.(type) {
		case map[string]any:
			from, name, isRef, err := reference(value)
			if err != nil {
				return err
			}
			if isRef {
				if from == source {
					names[name] = true
				}
				return nil
			}
			for _, key := range slices.Sorted(maps.Keys(value)) {
				if err := walk(value[key]); err != nil {
					return err
				}
			}
		case []any:
			for _, v := range value {
				if err := walk(v); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(value); err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(names)), nil
}
