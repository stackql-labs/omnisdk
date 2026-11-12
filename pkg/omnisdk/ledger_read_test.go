package omnisdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func TestLedgerReadDoesNotCreateState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	rows, err := (ledgerWAL{collection: "demo", state: root}).Read(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("reader created state", err)
	}
}

func TestLedgerReadScopePendingAndOmission(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	log, err := ledger.NewFile(filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []facade.LedgerKey{"demo/vm", "demo-other/vm", "demo/.lease"} {
		if err := log.Begin(ctx, key, []byte(`{"secret":"omit"}`), facade.LedgerVersionNone); err != nil {
			t.Fatal(err)
		}
		_, v, _, err := log.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Resolve(ctx, key, []byte("i-123"), []byte(`{"provider":"aws","address":"ec2.web_instance","params":{"secret":"omit"}}`), v); err != nil {
			t.Fatal(err)
		}
	}
	_, v, _, err := log.Get(ctx, "demo/vm")
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Begin(ctx, "demo/vm", []byte(`{"secret":"new"}`), v); err != nil {
		t.Fatal(err)
	}
	before, version, _, err := log.Get(ctx, "demo/vm")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := (ledgerWAL{collection: "demo", state: root}).Read(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	row := rows[0]
	if row["resource_key"] != "vm" || row["phase"] != "pending" || row["identity"] != "i-123" || row["version"] != string(version) {
		t.Fatal(row)
	}
	if _, present := row["params"]; present {
		t.Fatal("exposed parameters")
	}
	after, afterVersion, _, err := log.Get(ctx, "demo/vm")
	if err != nil {
		t.Fatal(err)
	}
	if afterVersion != version || string(after.Proposed()) != string(before.Proposed()) {
		t.Fatal("reader changed ledger")
	}
	children, err := os.ReadDir(root)
	if err != nil || len(children) != 1 || children[0].Name() != "ledger" {
		t.Fatal("reader created ancillary state", err)
	}
}
