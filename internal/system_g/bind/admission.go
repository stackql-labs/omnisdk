package bind

import (
	"context"
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type edgeStatesKey struct{}

func WithEdgeStates(ctx context.Context, edges map[string]facade.EdgeState) context.Context {
	return context.WithValue(ctx, edgeStatesKey{}, edges)
}

func ApplyInboundContext(ctx context.Context, t facade.Transform, inbox map[string]any, edges map[string]facade.EdgeState) (map[string]any, error) {
	contextual, ok := t.(facade.InboxTransform)
	if !ok {
		return applyInbound(t, inbox)
	}
	if override, ok := ctx.Value(edgeStatesKey{}).(map[string]facade.EdgeState); ok {
		edges = override
	}
	if admission := contextual.Admission(); admission != nil {
		admitted, err := admission.Admit(ctx, inbox, edges)
		if err != nil {
			return nil, err
		}
		if !admitted {
			return nil, fmt.Errorf("inbox admission was not satisfied")
		}
	}
	rec, err := contextual.ApplyInbox(ctx, inbox, edges)
	if err != nil {
		return nil, err
	}
	shaped, ok := DocMap(rec)
	if !ok {
		return nil, fmt.Errorf("inbound transform did not yield an object")
	}
	out := mergeMaps(inbox, shaped)
	return out, nil
}
