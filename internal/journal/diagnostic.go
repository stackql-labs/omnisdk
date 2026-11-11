package journal

import (
	"context"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Diagnostic describes execution evidence, never an instruction for recovery or compensation.
// Request/response bodies and credential-bearing request URLs are not stored.
type Diagnostic struct {
	Event        string `json:"event"`
	Stage        string `json:"stage"`
	Outcome      string `json:"outcome"`
	Message      string `json:"message,omitempty"`
	ProviderCode string `json:"provider_code,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
	Time         string `json:"time"
"strings"`
}

// AppendDiagnostic fsyncs execution evidence separately from the forward mutation steps.
func (r *runJournal) AppendDiagnostic(_ context.Context, key facade.LedgerKey, exchange string, diagnostic Diagnostic) error {
	diagnostic.Time = time.Now().UTC().Format(time.RFC3339Nano)
	r.mu.Lock()
	if r.diagnostics == nil {
		r.diagnostics = &runJournal{path: strings.TrimSuffix(r.path, ".jsonl") + ".diagnostics.jsonl"}
	}
	target := r.diagnostics
	r.mu.Unlock()
	_, err := target.appendLine(line{KeyName: key, XchgName: exchange, Diagnostic: &diagnostic})
	return err
}

// RecordDiagnostic supports journal implementations that expose diagnostic recording.
func RecordDiagnostic(ctx context.Context, log facade.Journal, key facade.LedgerKey, exchange string, diagnostic Diagnostic) error {
	if recorder, ok := log.(interface {
		AppendDiagnostic(context.Context, facade.LedgerKey, string, Diagnostic) error
	}); ok {
		return recorder.AppendDiagnostic(context.WithoutCancel(ctx), key, exchange, diagnostic)
	}
	return nil
}
