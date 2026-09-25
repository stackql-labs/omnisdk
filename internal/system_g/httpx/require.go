package httpx

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// requireOK fails the flow on a non-2xx response, carrying the status + body — so a list that
// errored (e.g. 403) surfaces loudly instead of silently decoding to zero rows. On 2xx it passes
// the response through unchanged.
type requireOK struct{}

// NewRequireOK builds a transform that errors on any non-2xx response.
func NewRequireOK() facade.Transform { return requireOK{} }

// StatusError is a response the provider sent with a non-2xx status. It is distinct from a request
// that got no response: the provider answered, so what it said about the request is known.
type StatusError struct {
	URL    string
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	if e.URL != "" {
		return fmt.Sprintf("httpx: request to %s failed: status %d: %s", e.URL, e.Status, e.Body)
	}
	return fmt.Sprintf("httpx: request failed: status %d: %s", e.Status, e.Body)
}

func (requireOK) Apply(in facade.Page) (facade.Record, error) {
	status := string(in.Bytes(KeyStatus))
	if status != "" && !strings.HasPrefix(status, "2") {
		code, _ := strconv.Atoi(status)
		return nil, &StatusError{URL: URL(in), Status: code, Body: string(in.Bytes(facade.AnonymousPayload))}
	}
	return record.NewRecord(map[string]facade.Value{
		KeyStatus:               value.NewBytesValue(in.Bytes(KeyStatus)),
		facade.AnonymousPayload: value.NewBytesValue(in.Bytes(facade.AnonymousPayload)),
	}), nil
}
