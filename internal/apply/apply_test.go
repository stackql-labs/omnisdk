package apply_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/apply"
	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/merge"
	"github.com/stackql-labs/omnisdk/internal/semantics"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/unwind"
)

// target records the mutation it was sent per key, and can be told to fail one exchange. It also
// exposes what the ledger held at the moment of each effect, which is how the write-ahead ordering
// is asserted rather than assumed.
type target struct {
	mu       sync.Mutex
	state    map[facade.LedgerKey]map[string]any
	calls    []string
	failOn   facade.LedgerKey
	log      facade.Ledger
	phaseAt  map[facade.LedgerKey]facade.LedgerPhase
	loggedAt map[facade.LedgerKey]bool
}

func newTarget() *target {
	return &target{
		state:    map[facade.LedgerKey]map[string]any{},
		phaseAt:  map[facade.LedgerKey]facade.LedgerPhase{},
		loggedAt: map[facade.LedgerKey]bool{},
	}
}

func (t *target) Effect(ctx context.Context, exchange string, k facade.LedgerKey, in facade.EffectInput) ([]byte, error) {
	mutation := in.Mutation
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, exchange+":"+string(k))

	// What did the durable log know when the wire call happened? Intent must already be recorded.
	if t.log != nil {
		e, _, found, err := t.log.Get(ctx, k)
		if err != nil {
			return nil, err
		}
		t.loggedAt[k] = found
		if found {
			t.phaseAt[k] = e.Phase()
		}
	}

	if k == t.failOn {
		return nil, errors.New("target refused")
	}
	var patch map[string]any
	if err := json.Unmarshal(mutation, &patch); err != nil {
		return nil, err
	}
	cur := t.state[k]
	if cur == nil {
		cur = map[string]any{}
	}
	for f, v := range patch {
		if v == nil {
			delete(cur, f)
			continue
		}
		cur[f] = v
	}
	t.state[k] = cur
	return []byte("id-" + k), nil
}

func (t *target) Read(_ context.Context, _ string, k facade.LedgerKey, _ facade.EffectInput) ([]byte, []byte, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, ok := t.state[k]
	if !ok {
		return nil, nil, true, nil
	}
	b, err := json.Marshal(cur)
	return b, []byte("id-" + k), true, err
}

func (t *target) called() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.calls)
}

func fixture(t *testing.T, tgt *target) (apply.Runner, facade.Ledger) {
	t.Helper()
	log, err := ledger.NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	tgt.log = log
	js, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatalf("journals: %v", err)
	}
	sem, err := semantics.New([]semantics.Declaration{
		// The stand-in target's create is an upsert, so it converges an existing object itself.
		{Exchange: "CreateVpc", Form: "insert", Inverse: "DeleteVpc", Fidelity: facade.FidelityExact, Update: "CreateVpc"},
		{Exchange: "CreateSubnet", Form: "insert", Inverse: "DeleteSubnet", Fidelity: facade.FidelityExact, Update: "CreateSubnet"},
	})
	if err != nil {
		t.Fatalf("semantics: %v", err)
	}
	leaser := lease.NewLeaser(log, time.Now)
	u := unwind.New(log, js, sem, tgt)
	return apply.New(log, js, leaser, merge.ThreeWay(), tgt, sem, u, lease.All(), time.Minute), log
}

func TestApplyLandsEveryStep(t *testing.T) {
	ctx := context.Background()
	tgt := newTarget()
	r, log := fixture(t, tgt)

	res, err := r.Apply(ctx, "run-1", "scope", []apply.Step{
		{Key: "vpc", Exchange: "CreateVpc", Desired: []byte(`{"cidr":"10.0.0.0/8"}`)},
		{Key: "subnet", Exchange: "CreateSubnet", Desired: []byte(`{"cidr":"10.0.1.0/24"}`)},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Complete() {
		t.Fatalf("result = %+v, want complete", res)
	}

	e, _, found, err := log.Get(ctx, "vpc")
	if err != nil || !found {
		t.Fatalf("get vpc: found=%v err=%v", found, err)
	}
	if e.Phase() != facade.LedgerLive {
		t.Errorf("phase = %v, want live", e.Phase())
	}
	if string(e.Plan()) != `{"cidr":"10.0.0.0/8"}` {
		t.Errorf("plan = %s, want the resolved intent", e.Plan())
	}
	if string(e.Identity()) != "id-vpc" {
		t.Errorf("identity = %s, want id-vpc", e.Identity())
	}
}

// Intent is recorded before the effect, and it is recorded as *pending*: that is the whole of the
// write-ahead discipline, and its absence is why a killed apply elsewhere leaves tainted resources.
func TestIntentIsPendingBeforeTheWireCall(t *testing.T) {
	ctx := context.Background()
	tgt := newTarget()
	r, _ := fixture(t, tgt)

	if _, err := r.Apply(ctx, "run-1", "scope", []apply.Step{
		{Key: "vpc", Exchange: "CreateVpc", Desired: []byte(`{"cidr":"10.0.0.0/8"}`)},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if !tgt.loggedAt["vpc"] {
		t.Error("no ledger entry existed when the effect was attempted")
	}
	if tgt.phaseAt["vpc"] != facade.LedgerPending {
		t.Errorf("phase at effect = %v, want pending", tgt.phaseAt["vpc"])
	}
}

// The ledger stores resolved intent; the mutation sent to the target is derived from it and the
// live read, and is never what gets persisted.
func TestSecondApplyUnsetsADroppedFieldWithoutStompingForeignOnes(t *testing.T) {
	ctx := context.Background()
	tgt := newTarget()
	r, _ := fixture(t, tgt)

	if _, err := r.Apply(ctx, "run-1", "scope", []apply.Step{
		{Key: "vpc", Exchange: "CreateVpc", Desired: []byte(`{"cidr":"10.0.0.0/8","dns":"ours"}`)},
	}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Another actor adds a field we have never mentioned.
	tgt.state["vpc"]["tag"] = "theirs"

	if _, err := r.Apply(ctx, "run-2", "scope", []apply.Step{
		{Key: "vpc", Exchange: "CreateVpc", Desired: []byte(`{"cidr":"10.0.0.0/8"}`)},
	}); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	got := tgt.state["vpc"]
	if _, present := got["dns"]; present {
		t.Error("dns survived, want it unset once we stopped asking for it")
	}
	if got["tag"] != "theirs" {
		t.Errorf("tag = %v, want a field we never managed left alone", got["tag"])
	}
}

func TestFailureUnwindsTheStepsThatLanded(t *testing.T) {
	ctx := context.Background()
	tgt := newTarget()
	tgt.failOn = "subnet"
	r, log := fixture(t, tgt)

	res, err := r.Apply(ctx, "run-1", "scope", []apply.Step{
		{Key: "vpc", Exchange: "CreateVpc", Desired: []byte(`{"cidr":"10.0.0.0/8"}`)},
		{Key: "subnet", Exchange: "CreateSubnet", Desired: []byte(`{"cidr":"10.0.1.0/24"}`)},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Complete() {
		t.Fatal("result reports complete, want the failure reported")
	}
	if res.Failed != "subnet" {
		t.Errorf("failed = %s, want subnet", res.Failed)
	}
	if res.Unwound == nil || !res.Unwound.Complete() {
		t.Fatalf("unwound = %+v, want a complete compensation", res.Unwound)
	}
	if !slices.Contains(tgt.called(), "DeleteVpc:vpc") {
		t.Errorf("calls = %v, want the landed step compensated", tgt.called())
	}
	if _, _, found, _ := log.Get(ctx, "vpc"); found {
		t.Error("ledger still holds vpc after compensation")
	}
}

// A second run cannot apply while the first holds the collection.
func TestConcurrentRunIsRefusedTheLease(t *testing.T) {
	ctx := context.Background()
	log, err := ledger.NewFile(t.TempDir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	leaser := lease.NewLeaser(log, time.Now)
	if _, err := leaser.Acquire(ctx, "scope", lease.All(), "other-runner", time.Minute); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}

	tgt := newTarget()
	js, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatalf("journals: %v", err)
	}
	sem, _ := semantics.New(nil)
	r := apply.New(log, js, leaser, merge.ThreeWay(), tgt, sem, unwind.New(log, js, sem, tgt), lease.All(), time.Minute)

	if _, err := r.Apply(ctx, "run-1", "scope", []apply.Step{
		{Key: "vpc", Exchange: "CreateVpc", Desired: []byte(`{}`)},
	}); !errors.Is(err, facade.ErrLeaseHeld) {
		t.Errorf("apply under a held lease = %v, want lease held", err)
	}
	if got := tgt.called(); len(got) != 0 {
		t.Errorf("calls = %v, want no effect attempted without the lease", got)
	}
}

// A run that updates an object an earlier run created, then fails, must restore the prior intent —
// not delete the object. Deleting there is destruction, not compensation: this run did not create
// it, and an earlier run's resource is not ours to remove.
func TestFailedUpdateRestoresRatherThanDeletes(t *testing.T) {
	ctx := context.Background()
	tgt := newTarget()
	r, log := fixture(t, tgt)

	if _, err := r.Apply(ctx, "run-1", "scope", []apply.Step{
		{Key: "vpc", Exchange: "CreateVpc", Desired: []byte(`{"cidr":"10.0.0.0/8","dns":"first"}`)},
	}); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Second run changes the existing vpc, then fails on a later step.
	tgt.failOn = "subnet"
	res, err := r.Apply(ctx, "run-2", "scope", []apply.Step{
		{Key: "vpc", Exchange: "CreateVpc", Desired: []byte(`{"cidr":"10.0.0.0/8","dns":"second"}`)},
		{Key: "subnet", Exchange: "CreateSubnet", Desired: []byte(`{"cidr":"10.0.1.0/24"}`)},
	})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res.Unwound == nil || !res.Unwound.Complete() {
		t.Fatalf("unwound = %+v, want a complete compensation", res.Unwound)
	}

	if _, gone := tgt.state["vpc"]; !gone {
		t.Fatal("the vpc was deleted; an update must be compensated by restoring, not removing")
	}
	if got := tgt.state["vpc"]["dns"]; got != "first" {
		t.Errorf("dns = %v, want the prior intent restored", got)
	}
	e, _, found, err := log.Get(ctx, "vpc")
	if err != nil || !found {
		t.Fatalf("vpc entry: found=%v err=%v", found, err)
	}
	if string(e.Plan()) != `{"cidr":"10.0.0.0/8","dns":"first"}` {
		t.Errorf("plan = %s, want the entry walked back to the prior intent", e.Plan())
	}
}
