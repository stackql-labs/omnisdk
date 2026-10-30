package omnisdk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/stackql-labs/omnisdk/internal/resourcebindings"
)

type bindingResource struct {
	ManagedResource
	rawParams map[string]any
	params    map[string]string
	desired   []byte
}

func (r bindingResource) Params() map[string]string { return maps.Clone(r.params) }
func (r bindingResource) Desired() []byte {
	if r.desired != nil {
		return slices.Clone(r.desired)
	}
	return r.ManagedResource.Desired()
}
func (r bindingResource) resourceParams() map[string]any { return r.rawParams }

func resourceParams(r ManagedResource) map[string]any {
	if raw, ok := r.(interface{ resourceParams() map[string]any }); ok {
		return raw.resourceParams()
	}
	out := map[string]any{}
	for k, v := range r.Params() {
		out[k] = v
	}
	return out
}

func ResourceEnvironmentVariables(resources []ManagedResource) ([]string, error) {
	seen := map[string]bool{}
	for _, r := range resources {
		desired, err := decodeObject(r.Desired())
		if err != nil {
			return nil, err
		}
		names, err := resourcebindings.New().References(map[string]any{"desired": desired, "write_once": resourceParams(r)}, "env")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Key(), err)
		}
		for _, name := range names {
			seen[name] = true
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

func bindingSnapshot(in map[string]any) (map[string]any, error) {
	if in == nil {
		return nil, nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("binding source is not JSON data: %w", err)
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	err = dec.Decode(&out)
	return out, err
}
func resolveResourceBindings(resources []ManagedResource, args Args, collection string) ([]ManagedResource, error) {
	inputs, err := bindingSnapshot(args.Inputs)
	if err != nil {
		return nil, err
	}
	env, err := bindingSnapshot(args.Environment)
	if err != nil {
		return nil, err
	}
	out := make([]ManagedResource, len(resources))
	for i, r := range resources {
		desired, err := decodeObject(r.Desired())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Key(), err)
		}
		if desired == nil {
			return nil, fmt.Errorf("%s: desired must be a JSON object", r.Key())
		}
		params := resourceParams(r)
		sources := map[string]map[string]any{
			"self":  {"key": r.Key(), "collection": collection, "provider": r.Provider(), "address": r.Address(), "desired": desired, "write_once": params},
			"input": inputs, "env": env,
		}
		resolved, err := resourcebindings.New().Resolve(map[string]any{"desired": desired, "write_once": params}, sources)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Key(), err)
		}
		resolvedObject := resolved.(map[string]any)
		raw, err := json.Marshal(resolvedObject["desired"])
		if err != nil {
			return nil, err
		}
		boundParams := map[string]string{}
		for _, k := range slices.Sorted(maps.Keys(resolvedObject["write_once"].(map[string]any))) {
			v := resolvedObject["write_once"].(map[string]any)[k]
			text, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s: write_once.%s must resolve to a string", r.Key(), k)
			}
			boundParams[k] = text
		}
		out[i] = bindingResource{ManagedResource: r, desired: raw, params: boundParams, rawParams: resolvedObject["write_once"].(map[string]any)}
	}
	return out, nil
}
