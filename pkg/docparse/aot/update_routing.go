package aot

// UpdateRoute retains a resource-bound recommendation and its resolved existing exchange.
// Returned slices must be immutable snapshots. Source and Order preserve declaration order.
type UpdateRoute interface {
	Resource() string
	Target() string
	Priority() int
	Requires() []string
	Permits() []string
	Source() string
	Location() string
	Order() int
	TargetResource() string
	Exchange() AOTExchange
}

// UpdateRouting is optional so existing dialects and Catalog implementations remain compatible.
// Its resource argument is a document resource name on Doc, or a published address on Catalog.
type UpdateRouting interface {
	UpdateRoutes(resource string) ([]UpdateRoute, error)
}
