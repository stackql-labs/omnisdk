package omnisdk

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange/docx"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func failureDiagnostic(stage string, err error) journal.Diagnostic {
	d := journal.Diagnostic{Event: "failure", Stage: stage, Outcome: "unknown", Message: diagnosticMessage(err.Error())}
	switch {
	case errors.Is(err, ErrRejected):
		d.Outcome = "rejected"
	case errors.Is(err, ErrNotAttempted):
		d.Outcome = "not_attempted"
	}
	var response facade.HTTPStatusError
	if errors.As(err, &response) {
		if response.StatusCode() >= 400 && response.StatusCode() < 500 {
			d.Outcome = "rejected"
		}
		// Extract provider error semantics, never persist the response document itself.
		var body struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
			Errors  struct {
				Error struct {
					Code    string `xml:"Code"`
					Message string `xml:"Message"`
				} `xml:"Error"`
			} `xml:"Errors"`
			RequestID string `xml:"RequestID"`
			RequestId string `xml:"RequestId"`
		}
		d.Message = fmt.Sprintf("provider returned HTTP status %d", response.StatusCode())
		if xml.Unmarshal([]byte(response.ResponseBody()), &body) == nil {
			d.ProviderCode = body.Code
			if d.ProviderCode == "" {
				d.ProviderCode = body.Errors.Error.Code
			}
			message := body.Message
			if message == "" {
				message = body.Errors.Error.Message
			}
			if message != "" {
				d.Message = diagnosticMessage(message)
			}
			d.RequestID = body.RequestID
			if d.RequestID == "" {
				d.RequestID = body.RequestId
			}
		}
		if d.ProviderCode == "" && d.Message == fmt.Sprintf("provider returned HTTP status %d", response.StatusCode()) {
			var body map[string]json.RawMessage
			if json.Unmarshal([]byte(response.ResponseBody()), &body) == nil {
				var nested map[string]json.RawMessage
				if json.Unmarshal(body["error"], &nested) == nil && nested != nil {
					body = nested
				}
				text := func(names ...string) string {
					for _, name := range names {
						var value string
						if json.Unmarshal(body[name], &value) == nil && value != "" {
							return value
						}
					}
					return ""
				}
				d.ProviderCode = text("code", "Code", "__type")
				if message := text("message", "Message", "error"); message != "" {
					d.Message = diagnosticMessage(message)
				}
				d.RequestID = text("requestId", "request_id", "RequestId", "RequestID")
			}
		}
	}
	return d
}

func diagnosticMessage(message string) string {
	// Keep useful local errors while removing signed query strings and URL credentials.
	pattern := regexp.MustCompile(`https?://[^\s<>"']+`)
	message = pattern.ReplaceAllStringFunc(message, func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "[request URL]"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	})
	if len(message) > 8192 {
		message = message[:8192] + " [truncated]"
	}
	return message
}

func recordRunFailure(ctx context.Context, h *held, key facade.LedgerKey, err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(err, journal.RecordDiagnostic(ctx, h.journal, key, "", failureDiagnostic("run", err)))
}

func (r *durableRecords) recordOutcome(ctx context.Context) {
	operation := r.inner.op.e
	diagnostic := journal.Diagnostic{Event: "success", Stage: "commit", Outcome: "committed"}
	if r.err != nil {
		diagnostic = failureDiagnostic("effect", r.err)
	}
	// Remove credential values available in the operation's inbox from diagnostic text.
	for name, value := range r.inner.op.bound {
		if docx.IsCredential(name) && value != nil && fmt.Sprint(value) != "" {
			diagnostic.Message = strings.ReplaceAll(diagnostic.Message, fmt.Sprint(value), "[redacted]")
		}
	}
	r.err = errors.Join(r.err, journal.RecordDiagnostic(ctx, r.d.run.journal, r.key, operation.exchange, diagnostic))
}
