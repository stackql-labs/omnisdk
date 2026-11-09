package stackqldoc

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

func (d *document) isCreate(resource, method string) bool {
	r := d.Components.Resources[resource]
	for _, declaration := range r.SQLVerbs["insert"] {
		if lastSegment(declaration.Ref) == method {
			return true
		}
	}
	return len(r.SQLVerbs["insert"]) == 0 && (method == "insert" || method == "create")
}

func (d *document) lifecycle(resource string, spec *lifecycleSpec) (*aot.Lifecycle, error) {
	if spec.StateDerivation == "none" {
		return nil, nil
	}
	if spec.StateDerivation != "self_read_returns_data" {
		return nil, fmt.Errorf("unsupported state_derivation %q", spec.StateDerivation)
	}
	interval, err := time.ParseDuration(spec.Interval)
	if err != nil || interval <= 0 {
		return nil, fmt.Errorf("interval must be a positive duration")
	}
	if spec.MaxAttempts <= 0 {
		return nil, fmt.Errorf("max_attempts must be positive")
	}
	// A collection listing is not a self read: some other object's existence
	// cannot establish that this create is observable.
	var read aot.AOTExchange
	methods := d.Components.Resources[resource].Methods
	var keys []string
	if _, exists := methods["get"]; exists {
		keys = append(keys, "get")
	}
	for _, declaration := range d.Components.Resources[resource].SQLVerbs["select"] {
		key := lastSegment(declaration.Ref)
		if !strings.Contains(strings.ToLower(key), "list") && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	for _, key := range []string{"select", "find", "retrieve"} {
		if _, exists := methods[key]; exists && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		candidate, err := d.resolve(resource, key, false)
		if err != nil {
			return nil, err
		}
		if candidate.Request().Method() != "GET" {
			continue
		}
		read = candidate
		break
	}
	if read == nil {
		return nil, fmt.Errorf("self_read_returns_data needs a get/select detail method; declare lifecycle.state_derivation: none to disable")
	}
	return &aot.Lifecycle{Method: read, StateDerivation: spec.StateDerivation, Bind: maps.Clone(spec.Bind), Interval: interval, MaxAttempts: spec.MaxAttempts}, nil
}
