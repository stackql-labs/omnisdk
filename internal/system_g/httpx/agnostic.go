package httpx

import (
	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// MakeAgnostic adapts a generic httpx exchange (which emits {status, raw body}) into one whose
// output is an agnostic {status, raw} record — so the bind join can consume it. reqT is the
// request-path transform chain (empty for none/bearer; a SigV4 transform for AWS). Continuation
// defaults to JSON; an XML paginator injects its decoder into Make directly instead.
func MakeAgnostic(req Request, reqT ...facade.Transform) func(map[string]any) facade.Operator {
	return func(bound map[string]any) facade.Operator {
		return exchange.NewTransformExchange(0, Make(req, nil, reqT...)(bound), NewResponseToAgnostic(), 1)
	}
}

// responseToAgnostic converts a raw HTTP response page (status + payload bytes) into an agnostic
// {status, raw} map WITHOUT any domain decoding — so the raw wire body survives downstream (to
// the sink, and to the log). Generic; no provider specifics.
type responseToAgnostic struct{}

// NewResponseToAgnostic builds the response→{status, raw} transform.
func NewResponseToAgnostic() facade.Transform {
	return responseToAgnostic{}
}

func (t responseToAgnostic) Apply(in facade.Page) (facade.Record, error) {
	return bind.NewDocRecord(map[string]any{
		"status":    readKey(in, KeyStatus),
		bind.KeyRaw: readKey(in, facade.AnonymousPayload),
		bind.KeyTag: bind.TagRaw, // presentation: log this slot's raw verbatim
	}), nil
}

// NewJSONExtract is the common case: a JSON response, 200 or fail. It pulls fields by dotted path
// onto the row; a non-200 fails the flow. It is NewStatusBranch{200: NewExtract(JSON)} — the
// canonical primitives, named for the frequent shape.
func NewJSONExtract(fields map[string]string) facade.Transform {
	return NewStatusBranch(map[string]facade.Transform{
		"200": NewExtract(nil, fields, nil),
	}, nil)
}

// readKey reads a page value as a string ("" if absent).
func readKey(p facade.Page, key string) string {
	return string(p.Bytes(key))
}
