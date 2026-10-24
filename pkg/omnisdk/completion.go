package omnisdk

import (
	"context"
	"fmt"
)

// MutationCompletion follows an accepted mutation before RETURNING projection.
// Complete receives its resource address, verb, bound inputs and initial response.
// It must honor cancellation; a nil result emits no row. Completion failure never
// retries the mutation and is reported as an unknown outcome.
// Implementations must support independent concurrent calls.
// Hosts supply this interface; Omni performs no storage or database discovery.
type MutationCompletion interface {
	Complete(context.Context, string, string, map[string]any, Row) (Row, error)
}

type completionBridge struct{ completion MutationCompletion }

func (b completionBridge) Complete(ctx context.Context, address, verb string, inputs, response map[string]any) (map[string]any, error) {
	done, err := b.completion.Complete(ctx, address, verb, inputs, Row(response))
	if err != nil {
		return nil, fmt.Errorf("omnisdk: %s %s completion failed (%w): %w", verb, address, ErrOutcomeUnknown, err)
	}
	return map[string]any(done), nil
}
