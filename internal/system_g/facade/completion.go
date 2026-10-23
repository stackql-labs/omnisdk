package facade

import "context"

// MutationCompletion follows an accepted mutation before its response is projected.
// Implementations honor cancellation and own completion discovery and polling.
// Implementations support independent concurrent calls.
// Inputs and response are private copies. A nil response emits no row.
type MutationCompletion interface {
	Complete(context.Context, string, string, map[string]any, map[string]any) (map[string]any, error)
}
