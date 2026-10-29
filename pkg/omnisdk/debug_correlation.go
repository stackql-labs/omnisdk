package omnisdk

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

type DebugCorrelationParam struct {
	Target      string `json:"target,omitempty"`
	Transformer string `json:"transformer,omitempty"`
}

type debugConfiguredResource struct {
	attributes []string
	ManagedResource
	config  DebugCorrelationParam
	desired []byte
	params  map[string]string
}

func (r debugConfiguredResource) DebugCorrelationParam() DebugCorrelationParam { return r.config }
func (r debugConfiguredResource) Desired() []byte {
	if r.desired != nil {
		return slices.Clone(r.desired)
	}
	return r.ManagedResource.Desired()
}
func (r debugConfiguredResource) Params() map[string]string {
	if r.params != nil {
		return maps.Clone(r.params)
	}
	return r.ManagedResource.Params()
}
func WithDebugCorrelationParam(r ManagedResource, config DebugCorrelationParam) ManagedResource {
	return debugConfiguredResource{ManagedResource: r, config: config}
}
func validateDebugCorrelation(c DebugCorrelationParam) error {
	if c.Transformer != "" && c.Transformer != "standard" {
		return fmt.Errorf("debug_correlation_param: unknown transformer %q", c.Transformer)
	}
	return nil
}

func bindDebugCorrelation(r ManagedResource, cat aot.Catalog, addr, collection string) (ManagedResource, error) {
	config := r.DebugCorrelationParam()
	if err := validateDebugCorrelation(config); err != nil {
		return nil, err
	}
	var bindings []aot.DebugCorrelationBinding
	if provider, ok := cat.(aot.DebugCorrelations); ok {
		var err error
		bindings, err = provider.DebugCorrelationBindings(addr)
		if err != nil {
			return nil, err
		}
	}
	desired, err := decodeObject(r.Desired())
	if err != nil {
		return nil, err
	}
	params := maps.Clone(r.Params())
	if params == nil {
		params = map[string]string{}
	}
	found := config.Target == ""
	var active facade.DebugCorrelationBinding
	var attributes []string
	for _, b := range bindings {
		var binding facade.DebugCorrelationBinding = b
		attributes = append(attributes, binding.Attribute())
		if _, conflict := desired[binding.Attribute()]; conflict {
			return nil, fmt.Errorf("%s: %s is supplied by debug_correlation_param", r.Key(), binding.Attribute())
		}
		desired[binding.Attribute()] = nil
		for k, v := range binding.CreateParams() {
			if given, ok := params[k]; ok && given != v {
				return nil, fmt.Errorf("%s: debug parameter %s conflicts with its document binding", r.Key(), k)
			}
			delete(params, k)
		}
		delete(params, binding.Target())
		if binding.Target() == config.Target {
			found = true
			active = binding
			desired[binding.Attribute()] = collection + "/" + r.Key()
		}
	}
	if !found {
		return nil, fmt.Errorf("%s: document declares no debug correlation binding for target %q", r.Key(), config.Target)
	}
	if active != nil {
		maps.Copy(params, active.CreateParams())
		params[active.Target()] = collection + "/" + r.Key()
	}
	raw, err := json.Marshal(desired)
	if err != nil {
		return nil, err
	}
	return debugConfiguredResource{ManagedResource: r, config: config, desired: raw, params: params, attributes: attributes}, nil
}
