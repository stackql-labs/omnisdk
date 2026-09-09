package journal_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func open(t *testing.T, root, run string) facade.Journal {
	t.Helper()
	js, err := journal.NewFiles(root)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	j, err := js.For(context.Background(), run)
	if err != nil {
		t.Fatalf("for %s: %v", run, err)
	}
	return j
}

func TestRecordsComeBackInAppendOrder(t *testing.T) {
	ctx := context.Background()
	j := open(t, t.TempDir(), "run-1")
	for _, k := range []facade.LedgerKey{"vpc", "subnet", "vm"} {
		if _, err := j.Append(ctx, k, "Create"+string(k), facade.FormCreate, nil); err != nil {
			t.Fatalf("append %s: %v", k, err)
		}
	}

	recs, err := j.Records(ctx)
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	want := []facade.LedgerKey{"vpc", "subnet", "vm"}
	if len(recs) != len(want) {
		t.Fatalf("len = %d, want %d", len(recs), len(want))
	}
	for i, w := range want {
		if recs[i].Key() != w || recs[i].Seq() != i+1 {
			t.Errorf("record %d = %s seq %d, want %s seq %d", i, recs[i].Key(), recs[i].Seq(), w, i+1)
		}
	}
}

// The order must survive the process, because a crashed run is torn down by a different one.
func TestOrderSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	first := open(t, root, "run-1")
	for _, k := range []facade.LedgerKey{"a", "b"} {
		if _, err := first.Append(ctx, k, "Create", facade.FormCreate, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	recs, err := open(t, root, "run-1").Records(ctx)
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	if len(recs) != 2 || recs[0].Key() != "a" || recs[1].Key() != "b" {
		t.Errorf("records = %v, want a then b after reopen", recs)
	}
}

// A reopened journal must continue the sequence rather than restart it, or two records share an
// ordinal and the reverse order is ambiguous.
func TestSequenceContinuesAfterReopen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := open(t, root, "run-1").Append(ctx, "a", "Create", facade.FormCreate, nil); err != nil {
		t.Fatalf("append: %v", err)
	}

	seq, err := open(t, root, "run-1").Append(ctx, "b", "Create", facade.FormCreate, nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if seq != 2 {
		t.Errorf("seq = %d, want 2 continuing the existing journal", seq)
	}
}

func TestRunsAreIndependent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if _, err := open(t, root, "run-1").Append(ctx, "a", "Create", facade.FormCreate, nil); err != nil {
		t.Fatalf("append: %v", err)
	}

	recs, err := open(t, root, "run-2").Records(ctx)
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("run-2 sees %d records, want its own empty journal", len(recs))
	}
}

func TestConcurrentAppendsAreAllRecordedWithDistinctSequences(t *testing.T) {
	ctx := context.Background()
	j := open(t, t.TempDir(), "run-1")

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			if _, err := j.Append(ctx, facade.LedgerKey(string(rune('a'+i%26))), "Create", facade.FormCreate, nil); err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	wg.Wait()

	recs, err := j.Records(ctx)
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	if len(recs) != n {
		t.Fatalf("len = %d, want %d", len(recs), n)
	}
	seen := map[int]bool{}
	for _, r := range recs {
		if seen[r.Seq()] {
			t.Errorf("duplicate sequence %d", r.Seq())
		}
		seen[r.Seq()] = true
	}
}

func TestIllegalRunIDIsRejected(t *testing.T) {
	js, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for _, id := range []string{"", "../escape", "a/b", "a.b"} {
		if _, err := js.For(context.Background(), id); err == nil {
			t.Errorf("For(%q) = nil, want rejection", id)
		}
	}
}
