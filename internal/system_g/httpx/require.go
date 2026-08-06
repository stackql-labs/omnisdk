package httpx

import (
	"fmt"
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

func (requireOK) Apply(in facade.Page) (facade.Record, error) {
	status := string(in.Bytes(KeyStatus))
	if status != "" && !strings.HasPrefix(status, "2") {
		if site := URL(in); site != "" {
			return nil, fmt.Errorf("httpx: request to %s failed: status %s: %s", site, status, string(in.Bytes(facade.AnonymousPayload)))
		}
		return nil, fmt.Errorf("httpx: request failed: status %s: %s", status, string(in.Bytes(facade.AnonymousPayload)))
	}
	return record.NewRecord(map[string]facade.Value{
		KeyStatus:               value.NewBytesValue(in.Bytes(KeyStatus)),
		facade.AnonymousPayload: value.NewBytesValue(in.Bytes(facade.AnonymousPayload)),
	}), nil
}
