package ledger_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func versionFiles(t *testing.T, root, dir, stem string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), stem+".v") {
			out = append(out, e.Name())
		}
	}
	return out
}

// Retention is the irreversible decision, so nothing is written over in place. Every mutation
// leaves its predecessor readable, which is what makes n-k rewind reachable later at no cost now.
func TestFileKeepsEveryVersion(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	l, err := ledger.NewFile(root)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, v := get(t, l, "s/a")
	if err := l.Resolve(ctx, "s/a", []byte("id-1"), v); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := versionFiles(t, root, "s", "a"); len(got) != 2 {
		t.Errorf("version files = %v, want v1 and v2 both retained", got)
	}
}

// Forget is a tombstone, not an unlink: the key reads as absent while its history survives.
func TestFileForgetIsATombstone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	l, err := ledger.NewFile(root)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, v := get(t, l, "s/a")
	if err := l.Forget(ctx, "s/a", v); err != nil {
		t.Fatalf("forget: %v", err)
	}

	if _, _, ok, err := l.Get(ctx, "s/a"); err != nil || ok {
		t.Errorf("get = ok:%v err:%v, want absent", ok, err)
	}
	if got := versionFiles(t, root, "s", "a"); len(got) != 2 {
		t.Errorf("version files = %v, want the pre-delete version retained", got)
	}
}

// A tombstoned key may be created again, and the new version must follow the tombstone rather
// than colliding with it.
func TestFileRecreateAfterForget(t *testing.T) {
	ctx := context.Background()
	l, err := ledger.NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := l.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, v := get(t, l, "s/a")
	if err := l.Forget(ctx, "s/a", v); err != nil {
		t.Fatalf("forget: %v", err)
	}

	if err := l.Begin(ctx, "s/a", []byte("again"), facade.LedgerVersionNone); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	e, _ := get(t, l, "s/a")
	if string(e.Proposed()) != "again" {
		t.Errorf("proposed = %q, want again", e.Proposed())
	}
}

// Keys are paths, so a segment that escapes the root must be refused rather than resolved.
func TestFileRejectsEscapingKeys(t *testing.T) {
	ctx := context.Background()
	l, err := ledger.NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for _, k := range []facade.LedgerKey{"../escape", "s/../../escape", "s//a", ""} {
		if err := l.Begin(ctx, k, []byte("n"), facade.LedgerVersionNone); err == nil {
			t.Errorf("begin %q = nil, want rejection", k)
		}
	}
}

// State must survive the process that wrote it; a second handle over the same root sees it.
func TestFileSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	first, err := ledger.NewFile(root)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := first.Begin(ctx, "s/a", []byte("n"), facade.LedgerVersionNone); err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, v := get(t, first, "s/a")
	if err := first.Resolve(ctx, "s/a", []byte("id-1"), v); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	second, err := ledger.NewFile(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	e, _ := get(t, second, "s/a")
	if string(e.Plan()) != "n" || string(e.Identity()) != "id-1" {
		t.Errorf("plan=%q identity=%q, want n / id-1 after reopen", e.Plan(), e.Identity())
	}
}
