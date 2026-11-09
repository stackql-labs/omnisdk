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

func TestCreateReservesRunEvenWithoutEffects(t *testing.T) {
	ctx := context.Background()
	js, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		wg.Go(func() { _, err := js.Create(ctx, "same-run"); results <- err })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("%d runs claimed one journal, want 1", successes)
	}
	j, err := js.For(ctx, "same-run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(ctx, "one", "create", facade.FormCreate, nil); err != nil {
		t.Fatal(err)
	}
	records, err := j.Records(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("recovery cannot reopen journal: records=%v err=%v", records, err)
	}
}

func TestRecoveryAboutSurvivesReopenAndRefusesConflicts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	appendAbout := func(run, key string, about []byte) {
		t.Helper()
		j := open(t, root, run).(interface {
			AppendAbout(context.Context, facade.LedgerKey, string, facade.FormClass, []byte, []byte) (int, error)
		})
		if _, err := j.AppendAbout(ctx, facade.LedgerKey(key), "create", facade.FormCreate, nil, about); err != nil {
			t.Fatal(err)
		}
	}
	about := []byte(`{"address":"things.widgets"}`)
	appendAbout("first", "demo/widget", about)
	appendAbout("other-collection", "other/widget", []byte(`{"address":"wrong"}`))
	got, err := journal.RecoveryAbout(ctx, root, "demo/widget")
	if err != nil || string(got) != string(about) {
		t.Fatalf("recovery=%s err=%v", got, err)
	}
	appendAbout("conflict", "demo/widget", []byte(`{"address":"other"}`))
	if _, err := journal.RecoveryAbout(ctx, root, "demo/widget"); err == nil {
		t.Fatal("conflicting metadata accepted")
	}
}

func TestAwaitCheckpointsPersistWithoutBecomingCompensationSteps(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	j := open(t, root, "run")
	if _, err := j.Append(ctx, "demo/db", "create", facade.FormCreate, nil); err != nil {
		t.Fatal(err)
	}
	if err := j.AppendAwait(ctx, "demo/db", "get_operation", map[string]any{"operation": "op-1", "project": "demo"}); err != nil {
		t.Fatal(err)
	}
	reopened := open(t, root, "run")
	records, err := reopened.Records(ctx)
	if err != nil || len(records) != 1 || records[0].Exchange() != "create" {
		t.Fatalf("compensation records=%v err=%v", records, err)
	}
	handles, err := journal.AwaitHandles(ctx, root, "run")
	if err != nil || handles["demo/db"].Inputs["operation"] != "op-1" {
		t.Fatalf("handles=%v err=%v", handles, err)
	}
}
