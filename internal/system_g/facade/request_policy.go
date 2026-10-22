package facade

import "context"

// RequestPolicy resolves the request settings for a provider within a run.
// Implementations share concurrency and retry state across that provider's calls.
type RequestPolicy interface {
	ForProvider(context.Context, string) context.Context
}
