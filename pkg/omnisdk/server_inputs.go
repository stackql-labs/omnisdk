package omnisdk

import (
	"fmt"
	"maps"
	"slices"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// ServerInputResolver is the common endpoint policy for query, EXEC and metadata callers.
type ServerInputResolver = facade.ServerInputResolver

type serverInputMetadata struct {
	specs     map[string]ServerVariableSpec
	transport map[string]string
}

type serverInputResolver struct {
	methods  []serverInputMetadata
	defaults map[string]string
	lookup   func(string) (string, bool)
}

// NewMethodInputResolver resolves explicit inputs before session defaults, declared
// environment sources/selectors, then transport defaults. Scope defaults are never inferred.
func NewMethodInputResolver(methods []MethodSignature, defaults map[string]string, lookup func(string) (string, bool)) ServerInputResolver {
	r := &serverInputResolver{defaults: map[string]string{}, lookup: lookup}
	for _, method := range methods {
		metadata := serverInputMetadata{specs: map[string]ServerVariableSpec{}}
		if declared, ok := method.(ServerVariableSpecs); ok {
			metadata.specs = declared.ServerVariableSpecs()
			metadata.specs = cloneInputSpecs(metadata.specs)
		}
		if declared, ok := method.(ServerEnvironmentBindings); ok {
			for name, environment := range declared.ServerEnvironmentBindings() {
				if _, exists := metadata.specs[name]; !exists {
					metadata.specs[name] = ServerVariableSpec{Environment: environment}
				}
			}
		}
		if declared, ok := method.(ServerTransportDefaults); ok {
			metadata.transport = maps.Clone(declared.ServerTransportDefaults())
		}
		for _, parameter := range method.Params() {
			if value, exists := defaults[parameter.Name()]; exists {
				r.defaults[parameter.Name()] = value
			}
		}
		r.methods = append(r.methods, metadata)
	}
	return r
}

func cloneInputSpecs(specs map[string]ServerVariableSpec) map[string]ServerVariableSpec {
	result := make(map[string]ServerVariableSpec, len(specs))
	for name, spec := range specs {
		if spec.Selector != nil {
			selector := *spec.Selector
			selector.Values = maps.Clone(selector.Values)
			spec.Selector = &selector
		}
		result[name] = spec
	}
	return result
}

// NewDocMethodInputResolver applies the same endpoint policy to SHOW METHODS.
func NewDocMethodInputResolver(method DocMethod, defaults map[string]string, lookup func(string) (string, bool)) ServerInputResolver {
	specs := cloneInputSpecs(method.ServerVariables)
	for name, environment := range method.ServerEnvironment {
		if _, exists := specs[name]; !exists {
			specs[name] = ServerVariableSpec{Environment: environment}
		}
	}
	filtered := map[string]string{}
	for _, name := range method.RequiredParams {
		if value, exists := defaults[name]; exists {
			filtered[name] = value
		}
	}
	return &serverInputResolver{methods: []serverInputMetadata{{specs: specs, transport: maps.Clone(method.TransportDefaults)}}, defaults: filtered, lookup: lookup}
}

func (r *serverInputResolver) Resolve(supplied func(string) bool) (map[string]string, error) {
	return r.resolve(supplied, true)
}

func (r *serverInputResolver) resolve(supplied func(string) bool, strict bool) (map[string]string, error) {
	resolved := map[string]string{}
	has := func(name string) bool { return supplied != nil && supplied(name) }
	for name, value := range r.defaults {
		if !has(name) {
			resolved[name] = value
		}
	}
	explicit := func(name string) bool { _, exists := r.defaults[name]; return has(name) || exists }
	for _, method := range r.methods {
		for _, name := range slices.Sorted(maps.Keys(method.specs)) {
			if explicit(name) {
				continue
			}
			resolver := NewServerVariableResolver(method.specs[name])
			if !resolver.Declared() {
				continue
			}
			value, exists, err := resolver.Resolve(name, r.lookup)
			if err != nil {
				return nil, err
			}
			if !exists {
				if strict {
					return nil, fmt.Errorf("server variable %q requires an input or its declared environment configuration", name)
				}
				continue
			}
			if prior, exists := resolved[name]; exists && prior != value {
				return nil, fmt.Errorf("conflicting server bindings for %s; supply an explicit binding", name)
			}
			resolved[name] = value
		}
	}
	// Resolve environment bindings across all methods before transport fallbacks.
	transport := map[string]string{}
	for _, method := range r.methods {
		for _, name := range slices.Sorted(maps.Keys(method.transport)) {
			if has(name) {
				continue
			}
			if _, exists := resolved[name]; exists {
				continue
			}
			value := method.transport[name]
			if value == "" {
				continue
			}
			// An unresolved declared source must not silently fall back to a default.
			declaredSource := false
			for _, candidate := range r.methods {
				if spec, exists := candidate.specs[name]; exists && NewServerVariableResolver(spec).Declared() {
					declaredSource = true
					break
				}
			}
			if declaredSource {
				continue
			}
			if prior, exists := transport[name]; exists && prior != value {
				return nil, fmt.Errorf("conflicting transport defaults for %s; supply an explicit binding", name)
			}
			transport[name] = value
		}
	}
	maps.Copy(resolved, transport)
	return resolved, nil
}

func (r *serverInputResolver) Required(names []string, supplied func(string) bool) ([]string, error) {
	resolved, err := r.resolve(supplied, false)
	if err != nil {
		return nil, err
	}
	required := make([]string, 0, len(names))
	for _, name := range names {
		if supplied != nil && supplied(name) {
			continue
		}
		if _, exists := resolved[name]; !exists {
			required = append(required, name)
		}
	}
	return required, nil
}
