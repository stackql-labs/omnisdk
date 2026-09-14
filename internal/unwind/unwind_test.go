package unwind_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/semantics"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/unwind"
)

// target is a stand-in for a provider. It enforces referential integrity the way a real one does —
// a parent cannot be deleted while a child exists — which is the behaviour unwind leans on instead
// of modelling containment itself.
type target struct {
	mu       sync.Mutex
	live     map[facade.LedgerKey]bool
	children map[facade.LedgerKey][]facade.LedgerKey
	calls    []string
	absent   map[facade.LedgerKey]bool
}

func newTarget(live ...facade.LedgerKey) *target {
	t := &target{live: map[facade.LedgerKey]bool{}, children: map[facade.LedgerKey][]facade.LedgerKey{}, absent: map[facade.LedgerKey]bool{}}
	for _, k := range live {
		t.live[k] = true
	}
	return t
}

func (t *target) Effect(_ context.Context, exchange string, k facade.LedgerKey, _ facade.EffectInput) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, exchange+":"+string(k))
	for _, c := range t.children[k] {
		if t.live[c] {
			return nil, fmt.Errorf("%w: %s still has child %s", facade.ErrCompensationBlocked, k, c)
		}
	}
	t.live[k] = false
	return []byte("id-" + k), nil
}

func (t *target) Read(_ context.Context, _ string, k facade.LedgerKey, _ facade.EffectInput) ([]byte, []byte, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.absent[k] || !t.live[k] {
		return nil, nil, true, nil
	}
	return []byte(`{"present":true}`), []byte("id-" + k), true, nil
}

func (t *target) called() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.calls)
}

func fixture(t *testing.T, tgt *target, decls []semantics.Declaration, steps [][2]string) unwind.Unwinder {
	t.Helper()
	ctx := context.Background()
	log, err := ledger.NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	js, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatalf("journals: %v", err)
	}
	j, err := js.For(ctx, "run-1")
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	sem, err := semantics.New(decls)
	if err != nil {
		t.Fatalf("semantics: %v", err)
	}
	for _, s := range steps {
		k, exch := facade.LedgerKey(s[0]), s[1]
		if _, err := j.Append(ctx, k, exch, facade.FormCreate, nil); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := log.Begin(ctx, k, []byte(`{"desired":true}`), facade.LedgerVersionNone); err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, v, _, err := log.Get(ctx, k)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if err := log.Resolve(ctx, k, []byte("id-"+k), v); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	return unwind.New(log, js, sem, tgt)
}

var creates = []semantics.Declaration{
	{Exchange: "CreateVpc", Form: "insert", Inverse: "DeleteVpc", Fidelity: facade.FidelityExact},
	{Exchange: "CreateSubnet", Form: "insert", Inverse: "DeleteSubnet", Fidelity: facade.FidelityExact},
	{Exchange: "CreateVm", Form: "insert", Inverse: "DeleteVm", Fidelity: facade.FidelityExact},
}

func TestCompensatesInReverseJournalOrder(t *testing.T) {
	tgt := newTarget("vpc", "subnet", "vm")
	u := fixture(t, tgt, creates, [][2]string{{"vpc", "CreateVpc"}, {"subnet", "CreateSubnet"}, {"vm", "CreateVm"}})

	out, err := u.Unwind(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("unwind: %v", err)
	}
	if !out.Complete() {
		t.Fatalf("outstanding = %v, want none", out.Outstanding)
	}
	want := []string{"DeleteVm:vm", "DeleteSubnet:subnet", "DeleteVpc:vpc"}
	if got := tgt.called(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// The ordering is a heuristic, so correctness must not depend on it. Here the journal order puts
// the parent last, so reversing it attempts the parent first and the provider refuses; the loop
// must retry rather than give up.
func TestFixpointRecoversFromWrongOrder(t *testing.T) {
	tgt := newTarget("vpc", "subnet")
	tgt.children["vpc"] = []facade.LedgerKey{"subnet"}
	u := fixture(t, tgt, creates, [][2]string{{"subnet", "CreateSubnet"}, {"vpc", "CreateVpc"}})

	out, err := u.Unwind(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("unwind: %v", err)
	}
	if !out.Complete() {
		t.Fatalf("outstanding = %v, want none once the child was removed", out.Outstanding)
	}
	want := []string{"DeleteVpc:vpc", "DeleteSubnet:subnet", "DeleteVpc:vpc"}
	if got := tgt.called(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want the blocked parent retried on a later pass: %v", got, want)
	}
}

// A loop that stalls terminates as partially compensated and says so, rather than looping or
// silently reporting success.
func TestStalledLoopReportsPartialCompensation(t *testing.T) {
	tgt := newTarget("vpc", "subnet")
	// The child is never removed, so the parent can never be deleted.
	tgt.children["vpc"] = []facade.LedgerKey{"subnet"}
	tgt.live["subnet"] = true
	u := fixture(t, tgt, []semantics.Declaration{
		{Exchange: "CreateVpc", Form: "insert", Inverse: "DeleteVpc", Fidelity: facade.FidelityExact},
		{Exchange: "CreateSubnet", Form: "insert"}, // no inverse: the child cannot be removed
	}, [][2]string{{"subnet", "CreateSubnet"}, {"vpc", "CreateVpc"}})

	out, err := u.Unwind(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("unwind: %v", err)
	}
	if out.Complete() {
		t.Fatal("outcome reports complete, want partially compensated")
	}
	if !slices.Contains(out.Outstanding, "subnet") || !slices.Contains(out.Outstanding, "vpc") {
		t.Errorf("outstanding = %v, want both keys", out.Outstanding)
	}
	if out.Reasons["subnet"] == "" {
		t.Error("outstanding key has no reason recorded")
	}
}

// Breaking a pending entry means reading the target first, never reversing blind: the call may
// never have landed.
func TestPendingEntryWithNothingAtTheTargetIsDropped(t *testing.T) {
	ctx := context.Background()
	tgt := newTarget()
	log, err := ledger.NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	js, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatalf("journals: %v", err)
	}
	j, _ := js.For(ctx, "run-1")
	if _, err := j.Append(ctx, "vpc", "CreateVpc", facade.FormCreate, nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Intent recorded, never resolved — the shape a crash between the log write and the response
	// leaves behind.
	if err := log.Begin(ctx, "vpc", []byte(`{"desired":true}`), facade.LedgerVersionNone); err != nil {
		t.Fatalf("begin: %v", err)
	}
	sem, _ := semantics.New(creates)

	out, err := unwind.New(log, js, sem, tgt).Unwind(ctx, "run-1")
	if err != nil {
		t.Fatalf("unwind: %v", err)
	}
	if !out.Complete() {
		t.Errorf("outstanding = %v, want none", out.Outstanding)
	}
	if got := tgt.called(); len(got) != 0 {
		t.Errorf("calls = %v, want no compensation for an effect that never landed", got)
	}
	if _, _, found, _ := log.Get(ctx, "vpc"); found {
		t.Error("pending entry survived, want it dropped")
	}
}

// unreadable is a target with no usable read: the log degrades from fact to belief, and whether an
// effect landed cannot be established.
type unreadable struct{ *target }

func (unreadable) Read(context.Context, string, facade.LedgerKey, facade.EffectInput) ([]byte, []byte, bool, error) {
	return nil, nil, false, nil
}

// A pending entry against an unreadable target must not be forgotten. Dropping it would discard the
// only record of a resource that may well exist, leaving it orphaned and unmanaged for good.
func TestPendingAgainstAnUnreadableTargetIsNotForgotten(t *testing.T) {
	ctx := context.Background()
	tgt := newTarget()
	log, err := ledger.NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	js, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatalf("journals: %v", err)
	}
	j, _ := js.For(ctx, "run-1")
	if _, err := j.Append(ctx, "vpc", "CreateVpc", facade.FormCreate, nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := log.Begin(ctx, "vpc", []byte(`{"desired":true}`), facade.LedgerVersionNone); err != nil {
		t.Fatalf("begin: %v", err)
	}
	sem, _ := semantics.New(creates)

	out, err := unwind.New(log, js, sem, unreadable{tgt}).Unwind(ctx, "run-1")
	if err != nil {
		t.Fatalf("unwind: %v", err)
	}
	if out.Complete() {
		t.Fatal("outcome reports complete, want the undecidable entry reported")
	}
	if !slices.Contains(out.Outstanding, "vpc") {
		t.Errorf("outstanding = %v, want the pending key reported", out.Outstanding)
	}
	if _, _, found, _ := log.Get(ctx, "vpc"); !found {
		t.Error("the entry was forgotten; it is the only record that the resource may exist")
	}
}
