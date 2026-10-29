// Package updaterouting evaluates changed-attribute sets without selecting or executing plans.
package updaterouting

import (
	"context"
	"fmt"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"slices"
	"strings"
)

type router struct{}

func New() facade.UpdateRouter { return router{} }
func Validate(r facade.UpdateRoute) error {
	if r == nil || r.Resource() == "" || r.Target() == "" {
		return fmt.Errorf("update route needs a resource and target")
	}
	allowed := map[string]bool{}
	for _, k := range r.Permits() {
		if k == "" || allowed[k] {
			return fmt.Errorf("update route %s: empty or duplicate permitted attribute %q", r.Location(), k)
		}
		allowed[k] = true
	}
	seen := map[string]bool{}
	for _, k := range r.Requires() {
		if k == "" || seen[k] || !allowed[k] {
			return fmt.Errorf("update route %s: requires must be unique and a subset of permits (%q)", r.Location(), k)
		}
		seen[k] = true
	}
	return nil
}
func (router) Candidates(ctx context.Context, changes map[string][]string, routes []facade.UpdateRoute) ([]facade.UpdateRoute, error) {
	var out []facade.UpdateRoute
	for _, r := range routes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := Validate(r); err != nil {
			return nil, err
		}
		changed := changes[r.Resource()]
		if len(changed) == 0 {
			continue
		}
		set := map[string]bool{}
		for _, k := range changed {
			if k == "" {
				return nil, fmt.Errorf("empty changed attribute for %s", r.Resource())
			}
			set[k] = true
		}
		if slices.ContainsFunc(r.Requires(), func(k string) bool { return !set[k] }) {
			continue
		}
		if slices.ContainsFunc(changed, func(k string) bool { return !slices.Contains(r.Permits(), k) }) {
			continue
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b facade.UpdateRoute) int {
		if a.Priority() != b.Priority() {
			if a.Priority() > b.Priority() {
				return -1
			}
			return 1
		}
		if n := strings.Compare(a.Source(), b.Source()); n != 0 {
			return n
		}
		if a.Order() < b.Order() {
			return -1
		}
		if a.Order() > b.Order() {
			return 1
		}
		if n := strings.Compare(a.Location(), b.Location()); n != 0 {
			return n
		}
		return strings.Compare(a.Resource(), b.Resource())
	})
	return out, ctx.Err()
}
