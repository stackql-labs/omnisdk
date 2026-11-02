package aot

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type EnvVarSelector struct {
	EnvVar  string            `json:"envVar,omitempty" yaml:"envVar"`
	Default string            `json:"default,omitempty" yaml:"default"`
	Values  map[string]string `json:"values" yaml:"values"`
}

type ServerVariableSpec struct {
	Default     string          `json:"default,omitempty" yaml:"default"`
	Environment string          `json:"x-stackQL-envVar,omitempty" yaml:"x-stackQL-envVar"`
	Selector    *EnvVarSelector `json:"x-stackQL-envVarSelector,omitempty" yaml:"x-stackQL-envVarSelector"`
}

type ServerVariableSpecs interface {
	ServerVariableSpecs() map[string]ServerVariableSpec
}

type VariableResolver = facade.VariableResolver

type serverVariableResolver struct{ spec ServerVariableSpec }

func CloneServerVariableSpecs(specs map[string]ServerVariableSpec) map[string]ServerVariableSpec {
	cloned := maps.Clone(specs)
	for name, spec := range cloned {
		cloned[name] = cloneServerVariableSpec(spec)
	}
	return cloned
}

func NewServerVariableResolver(spec ServerVariableSpec) VariableResolver {
	return serverVariableResolver{spec: cloneServerVariableSpec(spec)}
}

func cloneServerVariableSpec(spec ServerVariableSpec) ServerVariableSpec {
	if spec.Selector != nil {
		selector := *spec.Selector
		selector.Values = maps.Clone(selector.Values)
		spec.Selector = &selector
	}
	return spec
}

func (r serverVariableResolver) Declared() bool {
	return r.spec.Environment != "" || r.spec.Selector != nil
}

func (r serverVariableResolver) Resolve(name string, lookup func(string) (string, bool)) (string, bool, error) {
	spec := r.spec
	if spec.Selector != nil && spec.Selector.Default != "" {
		if _, exists := spec.Selector.Values[spec.Selector.Default]; !exists {
			return "", false, fmt.Errorf("server variable %q selector default %q is not in values %v", name, spec.Selector.Default, slices.Sorted(maps.Keys(spec.Selector.Values)))
		}
	}
	read := func(key string) string {
		if key == "" || lookup == nil {
			return ""
		}
		value, exists := lookup(key)
		if !exists {
			return ""
		}
		return strings.TrimSpace(value)
	}
	if value := read(spec.Environment); value != "" {
		value = strings.TrimRight(value, "/")
		if value == "" {
			return "", false, fmt.Errorf("server variable %q: environment variable %q resolves to an empty value", name, spec.Environment)
		}
		return value, true, nil
	}
	selector := spec.Selector
	if selector == nil {
		return "", false, nil
	}
	key := read(selector.EnvVar)
	if key == "" {
		key = selector.Default
	}
	if key == "" {
		return "", false, nil
	}
	value, exists := selector.Values[key]
	if !exists {
		return "", false, fmt.Errorf("server variable %q: environment variable %q value %q is not in values %v", name, selector.EnvVar, key, slices.Sorted(maps.Keys(selector.Values)))
	}
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	if value == "" {
		return "", false, fmt.Errorf("server variable %q selector entry %q is empty", name, key)
	}
	return value, true, nil
}
