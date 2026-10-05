package apply_test

import (
	"context"
	"strings"
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

// gated holds each effect until want are in flight at once, or a short wait passes, and records the
// most it saw together. It is how concurrency is observed rather than inferred from timing.
type gated struct {
	*target
	want     int
	mu       sync.Mutex
	cond     *sync.Cond
	inflight int
	peak     int
}

func newGated(tgt *target, want int) *gated {
	g := &gated{target: tgt, want: want}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *gated) Effect(ctx context.Context, exchange string, k facade.LedgerKey, in facade.EffectInput) ([]byte, error) {
	g.mu.Lock()
	g.inflight++
	g.peak = max(g.peak, g.inflight)
	g.cond.Broadcast()
	deadline := time.Now().Add(500 * time.Millisecond)
	for g.inflight < g.want && time.Now().Before(deadline) {
		stop := time.AfterFunc(10*time.Millisecond, g.cond.Broadcast)
		g.cond.Wait()
		stop.Stop()
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.inflight--
		g.mu.Unlock()
	}()
	return g.target.Effect(ctx, exchange, k, in)
}

func (g *gated) most() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peak
}

func runnerWith(t *testing.T, tgt *target, eff facade.Effector, parallelism int) apply.Runner {
	t.Helper()
	log, err := ledger.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tgt.log = log
	js, err := journal.NewFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sem, err := semantics.New([]semantics.Declaration{
		{Exchange: "CreateVpc", Form: "insert", Inverse: "DeleteVpc", Fidelity: facade.FidelityExact, Update: "CreateVpc"},
		{Exchange: "CreateSubnet", Form: "insert", Inverse: "DeleteSubnet", Fidelity: facade.FidelityExact, Update: "CreateSubnet"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return apply.New(log, js, lease.NewLeaser(log, time.Now), merge.ThreeWay(), eff, sem,
		unwind.New(log, js, sem, eff), lease.All(), time.Minute, parallelism)
}

func vpcs(keys ...string) []apply.Step {
	var out []apply.Step
	for _, k := range keys {
		out = append(out, apply.Step{Key: facade.LedgerKey(k), Exchange: "CreateVpc", Desired: []byte(`{"cidr":"10.0.0.0/8"}`)})
	}
	return out
}

// Keys with nothing between them converge at once.
func TestIndependentKeysConvergeTogether(t *testing.T) {
	tgt := newTarget()
	g := newGated(tgt, 4)
	res, err := runnerWith(t, tgt, g, 4).Apply(context.Background(), "run", "scope", vpcs("a", "b", "c", "d"))
	if err != nil || !res.Complete() {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if g.most() != 4 {
		t.Errorf("at most %d effects in flight, want all 4 together", g.most())
	}
}

// Parallelism bounds how many converge at once, and every key still lands.
func TestParallelismBoundsKeysInFlight(t *testing.T) {
	tgt := newTarget()
	g := newGated(tgt, 6)
	res, err := runnerWith(t, tgt, g, 2).Apply(context.Background(), "run", "scope", vpcs("a", "b", "c", "d", "e", "f"))
	if err != nil || !res.Complete() || len(res.Applied) != 6 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if g.most() > 2 {
		t.Errorf("%d effects in flight under parallelism 2", g.most())
	}
}

// A key waits for the key it reads from, wherever each is declared: a subnet listed before its VPC
// still runs after it.
func TestKeyWaitsForItsSource(t *testing.T) {
	tgt := newTarget()
	res, err := runnerWith(t, tgt, tgt, 4).Apply(context.Background(), "run", "scope", []apply.Step{
		{Key: "subnet", Exchange: "CreateSubnet", Desired: []byte(`{"cidr":"10.0.1.0/24"}`),
			Inbound: []apply.Arrival{{From: "vpc", As: "VpcId"}}},
		{Key: "vpc", Exchange: "CreateVpc", Desired: []byte(`{"cidr":"10.0.0.0/8"}`)},
	})
	if err != nil || !res.Complete() {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if got := strings.Join(tgt.called(), " "); got != "CreateVpc:vpc CreateSubnet:subnet" {
		t.Errorf("calls = %s, want the VPC first", got)
	}
}

// A cycle is refused before the lease is taken or anything is sent.
func TestDependencyCycleIsRefused(t *testing.T) {
	tgt := newTarget()
	_, err := runnerWith(t, tgt, tgt, 4).Apply(context.Background(), "run", "scope", []apply.Step{
		{Key: "a", Exchange: "CreateVpc", Desired: []byte(`{}`), Inbound: []apply.Arrival{{From: "b"}}},
		{Key: "b", Exchange: "CreateVpc", Desired: []byte(`{}`), Inbound: []apply.Arrival{{From: "a"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want a cycle", err)
	}
	if calls := tgt.called(); len(calls) != 0 {
		t.Errorf("calls = %v, want none", calls)
	}
}

// A failure stops new keys from starting; one already in flight finishes, and everything attempted is
// compensated.
func TestFailureStopsNewKeysAndUnwindsWhatLanded(t *testing.T) {
	tgt := newTarget()
	tgt.failOn = "a"
	g := newGated(tgt, 2)
	res, err := runnerWith(t, tgt, g, 2).Apply(context.Background(), "run", "scope", vpcs("a", "b", "c"))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Failed != "a" || res.Unwound == nil {
		t.Fatalf("res = %+v, want a failed and an unwind", res)
	}
	for _, c := range tgt.called() {
		if c == "CreateVpc:c" {
			t.Error("c started after a failed")
		}
	}
	// a's outcome is unknown, so it is compensated with b; c was never attempted, so it is not.
	got := map[facade.LedgerKey]bool{}
	for _, k := range res.Unwound.Compensated {
		got[k] = true
	}
	if !res.Unwound.Complete() || !got["a"] || !got["b"] || got["c"] {
		t.Errorf("unwound = %+v, want a and b compensated, c untouched", res.Unwound)
	}
}
