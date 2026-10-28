package omnisdk

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/analyze"
	"github.com/stackql-labs/omnisdk/internal/docsem"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/updaterouting"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

func (p *desiredPlan) routeDeclared(ctx context.Context, r ManagedResource, t *target, mutation, live map[string]any) (string, map[string]any, error) {
	var changed []string
	for k, v := range mutation {
		got, present := live[k]
		if (v == nil && present) || (v != nil && (!present || fmt.Sprint(v) != fmt.Sprint(got))) {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	rules := make([]facade.UpdateRoute, len(t.routes))
	for i, route := range t.routes {
		rules[i] = route
	}
	candidates, err := updaterouting.New().Candidates(ctx, map[string][]string{t.addr: changed}, rules)
	if err != nil {
		return "", nil, err
	}
	var reasons []string
	for _, candidate := range candidates {
		route := candidate.(aot.UpdateRoute)
		if route.TargetResource() != t.addr {
			reasons = append(reasons, fmt.Sprintf("%s requires target %s: declare its managed graph explicitly before this update", route.Target(), route.TargetResource()))
			continue
		}
		// Desired-state lowering already owns update arms and their durable semantics. A target whose
		// lifecycle is not declared as update/replace must be planned as its own explicit managed graph,
		// rather than silently relabelling a create/delete as an in-place update.
		index := slices.IndexFunc(t.updates, func(m facade.Method) bool { return m.Name() == route.Exchange().Name() })
		if index < 0 {
			reasons = append(reasons, route.Target()+": target is not a declared update/replace arm; supply its explicit managed graph")
			continue
		}
		method := t.updates[index]
		if err := facade.CheckAccess(ctx, "operation/"+t.cat.Provider().Name()+"/"+route.Exchange().Name()+"/"+route.Exchange().OperationID(), "execute"); err != nil {
			reasons = append(reasons, route.Target()+": "+err.Error())
			continue
		}
		if err := facade.CheckAccess(ctx, "credential/"+t.cat.Provider().Name(), "use"); err != nil {
			reasons = append(reasons, route.Target()+": "+err.Error())
			continue
		}

		send := mutation
		if !method.TakesBody() {
			send = map[string]any{}
			removed := false
			for _, k := range changed {
				if mutation[k] == nil {
					removed = true
					break
				}
				send[k] = mutation[k]
			}
			if removed {
				reasons = append(reasons, route.Target()+": an operation without a body cannot remove a field")
				continue
			}
		}
		sent := append(slices.Sorted(maps.Keys(send)), r.AddressedBy())
		available := append(slices.Clone(sent), slices.Collect(maps.Keys(p.args.Params))...)
		available = append(available, slices.Collect(maps.Keys(r.Params()))...)
		available = append(available, t.incoming...)
		selected, err := analyze.New(docsem.Capabilities(t.cat), analyze.PermitAll()).Analyze(ctx, analyze.NewStatement(principalOf(p.args), t.addr, t.updateVerb, available, sent, method.Name()))
		if err != nil {
			reasons = append(reasons, route.Target()+": "+err.Error())
			continue
		}
		return selected.Name(), send, nil
	}
	if len(reasons) == 0 {
		return "", nil, fmt.Errorf("no declared update route permits the complete change set %v", changed)
	}
	return "", nil, fmt.Errorf("no valid declared update target: %s", strings.Join(reasons, "; "))
}
