package facade

import "context"

// UpdateRoute is a recommendation, not permission to execute its target.
// Resource is one bound resource; changes from different resources are never combined.
type UpdateRoute interface {
	Resource() string
	Target() string
	Priority() int
	Requires() []string
	Permits() []string
	Source() string
	Location() string
	Order() int
}

// UpdateRouter returns every matching recommendation, ordered deterministically.
type UpdateRouter interface {
	Candidates(context.Context, map[string][]string, []UpdateRoute) ([]UpdateRoute, error)
}
