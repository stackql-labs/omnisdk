package journal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func TestDiagnosticsNeverBecomeRecoverySteps(t *testing.T) {
	root := t.TempDir()
	files, err := NewFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	log, err := files.Create(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ctx, "demo/vm", "create", facade.FormCreate, nil); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := RecordDiagnostic(cancelled, log, "demo/vm", "create", Diagnostic{Event: "failure", Stage: "effect", Outcome: "unknown", Message: "deadline exceeded"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := files.For(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	records, err := reopened.Records(ctx)
	if err != nil || len(records) != 1 || records[0].Form() != facade.FormCreate {
		t.Fatal(records, err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "run-1.diagnostics.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record line
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Diagnostic == nil || record.Diagnostic.Message != "deadline exceeded" || record.Diagnostic.Time == "" {
		t.Fatalf("%s", raw)
	}
	info, err := os.Stat(filepath.Join(root, "run-1.diagnostics.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatal("diagnostics file is not private")
	}
}
