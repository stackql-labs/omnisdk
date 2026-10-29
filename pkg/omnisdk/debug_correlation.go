package omnisdk

import "fmt"

// Legacy DTO retained so existing Go callers receive a migration error.
type DebugCorrelationParam struct {
	Target      string `json:"target,omitempty"`
	Transformer string `json:"transformer,omitempty"`
}
type debugConfiguredResource struct {
	ManagedResource
	config DebugCorrelationParam
}

func (r debugConfiguredResource) DebugCorrelationParam() DebugCorrelationParam { return r.config }
func WithDebugCorrelationParam(r ManagedResource, config DebugCorrelationParam) ManagedResource {
	return debugConfiguredResource{ManagedResource: r, config: config}
}
func validateDebugCorrelation(config DebugCorrelationParam) error {
	if config.Target != "" || config.Transformer != "" {
		return fmt.Errorf("debug_correlation_param is retired; use ordinary desired/params values with structured self, input or env references")
	}
	return nil
}
