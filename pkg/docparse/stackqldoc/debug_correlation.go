package stackqldoc

import (
	"fmt"
	"maps"
	"slices"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

type debugCorrelationBinding struct {
	TargetName     string            `yaml:"target"`
	StateAttribute string            `yaml:"attribute"`
	Params         map[string]string `yaml:"createParams"`
}

func (b debugCorrelationBinding) Target() string                  { return b.TargetName }
func (b debugCorrelationBinding) Attribute() string               { return b.StateAttribute }
func (b debugCorrelationBinding) CreateParams() map[string]string { return maps.Clone(b.Params) }

func (d *document) DebugCorrelationBindings(resource string) ([]aot.DebugCorrelationBinding, error) {
	r, ok := d.Components.Resources[resource]
	if !ok {
		return nil, fmt.Errorf("unknown resource %q", resource)
	}
	var result []aot.DebugCorrelationBinding
	var targets, attrs []string
	for _, b := range r.DebugCorrelations {
		if b.TargetName == "" || b.StateAttribute == "" || slices.Contains(targets, b.TargetName) || slices.Contains(attrs, b.StateAttribute) {
			return nil, fmt.Errorf("%s: debugCorrelations require unique nonempty targets and attributes", resource)
		}
		targets = append(targets, b.TargetName)
		attrs = append(attrs, b.StateAttribute)
		result = append(result, b)
	}
	return result, nil
}
func (c *catalog) DebugCorrelationBindings(address string) ([]aot.DebugCorrelationBinding, error) {
	_, service, resource, err := c.split(address)
	if err != nil {
		return nil, err
	}
	doc, err := c.doc(service)
	if err != nil {
		return nil, err
	}
	if dc, ok := doc.(aot.DebugCorrelations); ok {
		return dc.DebugCorrelationBindings(resource)
	}
	return nil, nil
}
