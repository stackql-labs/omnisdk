package omnisdk

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/updaterouting"
	"github.com/stackql-labs/omnisdk/pkg/docparse/aot"
)

// UpdateCandidate is a document recommendation. Discovering one neither selects nor executes it.
// TargetResource and TargetMethod can be used with the existing graph/node constructors.
type UpdateCandidate interface {
	Resource() string
	Target() string
	TargetResource() string
	TargetMethod() string
	Priority() int
	Requires() []string
	Permits() []string
	Source() string
	Location() string
}
type updateCandidate struct{ aot.UpdateRoute }

func (c updateCandidate) TargetMethod() string { return c.Exchange().Name() }

// UpdateCandidates evaluates each resource's complete changed-attribute set independently.
// Addresses are explicit published resource addresses. It performs no provider calls or mutations.
func UpdateCandidates(ctx context.Context, registry string, changes map[string][]string, args Args) ([]UpdateCandidate, error) {
	var all []aot.UpdateRoute
	for _, address := range slices.Sorted(maps.Keys(changes)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(changes[address]) == 0 {
			continue
		}
		cat, err := openDocs(registry, address, args.documentOptions()...)
		if err != nil {
			return nil, err
		}
		routing, ok := cat.(aot.UpdateRouting)
		if !ok {
			continue
		}
		routes, err := routing.UpdateRoutes(address)
		if err != nil {
			return nil, fmt.Errorf("omnisdk: update routes for %s: %w", address, err)
		}
		all = append(all, routes...)
	}
	rules := make([]facade.UpdateRoute, len(all))
	for i, r := range all {
		rules[i] = r
	}
	matched, err := updaterouting.New().Candidates(ctx, changes, rules)
	if err != nil {
		return nil, err
	}
	out := make([]UpdateCandidate, 0, len(matched))
	for _, r := range matched {
		out = append(out, updateCandidate{r.(aot.UpdateRoute)})
	}
	return out, nil
}
