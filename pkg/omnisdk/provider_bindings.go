package omnisdk

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/plan"
	"github.com/stackql-labs/omnisdk/internal/system_g/requestpolicy"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

type ProviderBinding struct {
	Provider   string            `json:"provider"`
	Tenancy    string            `json:"tenancy"`
	Auth       *Auth             `json:"auth,omitempty"`
	AuthSecret string            `json:"auth_secret,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
	Tuning     ProviderTuning    `json:"tuning,omitempty"`
}

func validBinding(alias string, b ProviderBinding, resolved bool) error {
	for _, name := range []string{alias, b.Provider} {
		if name == "" || strings.ContainsAny(name, "/\\.\x00") {
			return fmt.Errorf("provider alias, provider and tenancy must be explicit single identifiers")
		}
	}
	if b.Tenancy == "" || strings.ContainsAny(b.Tenancy, "/\\\x00\r\n") {
		return fmt.Errorf("tenancy must be an explicit identifier without path separators")
	}
	if b.Tuning.MaxPerHost < 0 || b.Tuning.RetryTries < 0 || b.Tuning.RetryRate < 0 {
		return fmt.Errorf("binding retry and per-host limits cannot be negative")
	}
	if b.Auth != nil && b.AuthSecret != "" {
		return fmt.Errorf("provider binding cannot specify both auth and auth_secret")
	}
	if resolved && (b.Auth == nil || b.AuthSecret != "") {
		return fmt.Errorf("provider binding %q requires resolved explicit credentials", alias)
	}
	return nil
}

func scopeName(provider, tenancy string) string {
	return strings.TrimPrefix(provider, aot.DefaultProviderPrefix) + "\x00" + tenancy
}
func (a Args) binding(address string) (string, Args, *ProviderBinding, error) {
	alias, rest, found := strings.Cut(address, ".")
	binding, bound := a.ProviderAliases[alias]
	if !found || !bound {
		return address, a, nil, nil
	}
	if err := validBinding(alias, binding, true); err != nil {
		return "", a, nil, err
	}
	a.AuthByProvider = maps.Clone(a.AuthByProvider)
	if a.AuthByProvider == nil {
		a.AuthByProvider = map[string]*Auth{}
	}
	provider := strings.TrimPrefix(binding.Provider, aot.DefaultProviderPrefix)
	a.AuthByProvider[provider] = binding.Auth
	a.AuthByProvider[aot.DefaultProviderPrefix+provider] = binding.Auth
	a.Params = maps.Clone(a.Params)
	if a.Params == nil {
		a.Params = map[string]string{}
	}
	maps.Copy(a.Params, binding.Params)
	a.TuningByProvider = maps.Clone(a.TuningByProvider)
	if a.TuningByProvider == nil {
		a.TuningByProvider = map[string]ProviderTuning{}
	}
	base, found := a.TuningByProvider[aot.DefaultProviderPrefix+provider]
	if !found {
		base = a.TuningByProvider[provider]
	}
	base = mergeProviderTuning(base, binding.Tuning)
	a.TuningByProvider[provider] = base
	a.TuningByProvider[aot.DefaultProviderPrefix+provider] = base
	if a.ReadCache != nil {
		if a.ReadCachePartition == "" {
			return "", a, nil, fmt.Errorf("tenancy caching requires an explicit caller cache partition")
		}
		partition, _ := json.Marshal([]string{a.ReadCachePartition, provider, binding.Tenancy, alias, a.bindingVersion})
		a.ReadCachePartition = string(partition)
	}
	return aot.DefaultProviderPrefix + provider + "." + rest, a, &binding, nil
}

func validateProviderBindings(a Args) error { return validateBindingSet(a.ProviderAliases, true) }
func validateBindingSet(bindings map[string]ProviderBinding, resolved bool) error {
	scopes := map[string]ProviderTuning{}
	for alias, binding := range bindings {
		if err := validBinding(alias, binding, resolved); err != nil {
			return err
		}
		key := scopeName(binding.Provider, binding.Tenancy)
		if previous, found := scopes[key]; found && previous != binding.Tuning {
			return fmt.Errorf("aliases for the same provider and tenancy must use identical tuning")
		}
		scopes[key] = binding.Tuning
	}
	return nil
}

type boundProviderNode struct {
	Node
	params map[string]string
}

func (n boundProviderNode) Params() map[string]string { return n.params }
func (n boundProviderNode) Tuples() []map[string]any {
	if tuples, ok := n.Node.(interface{ Tuples() []map[string]any }); ok {
		return tuples.Tuples()
	}
	return nil
}

type boundProviderGraph struct {
	Graph
	nodes []Node
}

func (g boundProviderGraph) Nodes() []Node { return g.nodes }
func bindProviderNodes(g Graph, args Args) (Graph, error) {
	if err := validateProviderBindings(args); err != nil {
		return nil, err
	}
	if len(args.ProviderAliases) == 0 {
		return g, nil
	}
	nodes := make([]Node, len(g.Nodes()))
	for i, n := range g.Nodes() {
		nodes[i] = n
		_, resolved, binding, err := args.binding(n.Address())
		if err != nil {
			return nil, err
		}
		if binding == nil {
			continue
		}
		params := maps.Clone(resolved.Params)
		for name := range g.Fanout() {
			delete(params, name)
		}
		for name := range n.Fanout() {
			delete(params, name)
		}
		if source, ok := g.(interface{ resolvedInputs() map[string]string }); ok {
			maps.Copy(params, source.resolvedInputs())
		}
		maps.Copy(params, n.Params())
		nodes[i] = boundProviderNode{Node: n, params: params}
	}
	return boundProviderGraph{Graph: g, nodes: nodes}, nil
}

type tenancyExchange struct {
	plan.ExchangeSpec
	provider, tenancy string
}

func (s tenancyExchange) Make(bound map[string]any) facade.Operator {
	return tenancyOperator{inner: s.ExchangeSpec.Make(bound), provider: s.provider, tenancy: s.tenancy}
}

type tenancyOperator struct {
	inner             facade.Operator
	provider, tenancy string
}

func (o tenancyOperator) Open(ctx context.Context) facade.Records {
	authorized, err := facade.Authorize(ctx, facade.Permission{Resource: "tenancy/" + o.provider + "/" + o.tenancy, Action: "use"})
	if err != nil {
		b := buffer.NewBuffer(1, 1, 0)
		b.Complete(err)
		return b.Reader()
	}
	return o.inner.Open(requestpolicy.WithTenancy(authorized, o.provider, o.tenancy))
}
func scopedExchange(spec plan.ExchangeSpec, binding *ProviderBinding) plan.ExchangeSpec {
	if binding == nil {
		return spec
	}
	return tenancyExchange{ExchangeSpec: spec, provider: strings.TrimPrefix(binding.Provider, aot.DefaultProviderPrefix), tenancy: binding.Tenancy}
}

type tenancyPlan struct {
	Plan
	args    Args
	binding ProviderBinding
}

func scopePlan(p Plan, args Args, binding *ProviderBinding) Plan {
	if binding == nil {
		return p
	}
	return tenancyPlan{Plan: p, args: args, binding: *binding}
}
func (p tenancyPlan) Open(ctx context.Context) (Rows, error) {
	ctx, err := executionContext(ctx, p.args)
	if err != nil {
		return nil, err
	}
	provider := strings.TrimPrefix(p.binding.Provider, aot.DefaultProviderPrefix)
	ctx, err = facade.Authorize(ctx, facade.Permission{Resource: "tenancy/" + provider + "/" + p.binding.Tenancy, Action: "use"})
	if err != nil {
		return nil, err
	}
	return p.Plan.Open(requestpolicy.WithTenancy(ctx, provider, p.binding.Tenancy))
}
