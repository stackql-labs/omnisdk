package omnisdk

import (
	"context"
	"errors"
	"fmt"
	"github.com/stackql-labs/omnisdk/internal/lease"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type faultLedger struct {
	facade.Ledger
	fail string
}

func (f *faultLedger) Begin(ctx context.Context, key facade.LedgerKey, data []byte, v facade.LedgerVersion) error {
	if f.fail == "intent" {
		return fmt.Errorf("injected intent failure")
	}
	return f.Ledger.Begin(ctx, key, data, v)
}
func (f *faultLedger) Resolve(ctx context.Context, key facade.LedgerKey, id, about []byte, v facade.LedgerVersion) error {
	if f.fail == "commit" {
		return fmt.Errorf("injected commit failure")
	}
	return f.Ledger.Resolve(ctx, key, id, about, v)
}

type faultJournal struct {
	facade.Journal
	fail bool
}

func (j faultJournal) AppendAbout(ctx context.Context, key facade.LedgerKey, exchange string, form facade.FormClass, prior, about []byte) (int, error) {
	if j.fail {
		return 0, fmt.Errorf("injected journal failure")
	}
	return j.Journal.AppendAbout(ctx, key, exchange, form, prior, about)
}

type countedEffect struct {
	calls   *int
	outcome string
}

func (o countedEffect) Open(ctx context.Context) facade.Records {
	*o.calls++
	if o.outcome == "lost" {
		return failed(fmt.Errorf("response lost after remote create"))
	}
	if o.outcome == "missing" {
		return staticOp{rows: []map[string]any{{"accepted": true}}}.Open(ctx)
	}
	return staticOp{rows: []map[string]any{{"id": "remote-1"}}}.Open(ctx)
}

func TestManagedLedgerCrashBoundaries(t *testing.T) {
	for _, fault := range []string{"intent", "journal", "lost", "missing", "commit", "success"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			disk, err := ledger.NewFile(filepath.Join(root, "ledger"))
			if err != nil {
				t.Fatal(err)
			}
			log := &faultLedger{Ledger: disk, fail: fault}
			js, err := journal.NewFiles(filepath.Join(root, "journal"))
			if err != nil {
				t.Fatal(err)
			}
			j, err := js.Create(ctx, "first")
			if err != nil {
				t.Fatal(err)
			}
			run := &managedRun{name: "iac", ledger: log, journal: faultJournal{Journal: j, fail: fault == "journal"}}
			d := durable{run: run, resource: NewManaged("vm", []string{"create"}, "id", "id")}
			calls := 0
			op := effectOp{inner: countedEffect{calls: &calls, outcome: fault}, bound: map[string]any{"name": "demo"}, e: effect{verb: "insert", alias: "create", exchange: "widgets.insert"}}
			rows := d.open(ctx, op)
			published := 0
			for rows.Next(ctx) {
				published++
			}
			gotErr := rows.Err()
			rows.Close()
			wantCalls := 1
			if fault == "intent" || fault == "journal" {
				wantCalls = 0
				if !errors.Is(gotErr, ErrNotAttempted) {
					t.Fatalf("outcome: %v", gotErr)
				}
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want %d", calls, wantCalls)
			}
			if fault == "lost" && !errors.Is(gotErr, ErrOutcomeUnknown) {
				t.Fatal(gotErr)
			}
			if (fault == "success") != (gotErr == nil) {
				t.Fatalf("error=%v", gotErr)
			}
			if (fault == "success") != (published == 1) {
				t.Fatalf("published %d rows before durable commit", published)
			}
			reopened, err := ledger.NewFile(filepath.Join(root, "ledger"))
			if err != nil {
				t.Fatal(err)
			}
			entry, _, found, err := reopened.Get(ctx, "iac/vm")
			if err != nil {
				t.Fatal(err)
			}
			if fault == "intent" {
				if found {
					t.Fatal("intent failure wrote entry")
				}
				return
			}
			if !found {
				t.Fatal("lost recovery evidence")
			}
			if fault == "success" {
				if entry.Phase() != facade.LedgerLive || string(entry.Identity()) != "remote-1" {
					t.Fatal("identity not durable")
				}
			} else if entry.Phase() != facade.LedgerPending {
				t.Fatal("uncertain mutation incorrectly live")
			}
			// Simulate a new process with a fresh run id. Neither unresolved intent nor
			// a live create may silently send another create, even with identical inputs.
			next, err := js.Create(ctx, "restart")
			if err != nil {
				t.Fatal(err)
			}
			run.ledger = reopened
			run.journal = next
			again := d.open(ctx, op)
			for again.Next(ctx) {
			}
			if again.Err() == nil {
				t.Fatal("replayed create accepted")
			}
			again.Close()
			if calls != wantCalls {
				t.Fatal("recovery duplicated remote create")
			}
		})
	}
}

type deleteEffect struct {
	facade.Effector
	calls int
	err   error
}

func (e *deleteEffect) Effect(context.Context, string, facade.LedgerKey, facade.EffectInput) ([]byte, error) {
	e.calls++
	return nil, e.err
}

func TestDeleteLostResponseRemainsPendingAfterRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	h, err := hold(ctx, root, "iac", "delete-first")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.log.Begin(ctx, "iac/vm", []byte(`{"name":"demo"}`), facade.LedgerVersionNone); err != nil {
		t.Fatal(err)
	}
	_, v, _, err := h.log.Get(ctx, "iac/vm")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.log.Resolve(ctx, "iac/vm", []byte("remote-1"), []byte(`{}`), v); err != nil {
		t.Fatal(err)
	}
	remote := &deleteEffect{err: fmt.Errorf("response lost after remote delete")}
	p := &destroyPlan{}
	if err := p.delete(ctx, h, "iac/vm", "widgets:delete", aboutRecord{}, []byte("remote-1"), remote); err == nil {
		t.Fatal("unknown deletion accepted")
	}
	h.release()
	restart, err := hold(ctx, root, "iac", "delete-restart")
	if err != nil {
		t.Fatal(err)
	}
	defer restart.release()
	entry, _, found, err := restart.log.Get(ctx, "iac/vm")
	if err != nil || !found {
		t.Fatal("recovery evidence lost:", err)
	}
	if entry.Phase() != facade.LedgerPending || string(entry.Identity()) != "remote-1" {
		t.Fatal("delete uncertainty not durable")
	}
	if err := p.delete(ctx, restart, "iac/vm", "widgets:delete", aboutRecord{}, []byte("remote-1"), remote); err == nil {
		t.Fatal("blind delete retry accepted")
	}
	if remote.calls != 1 {
		t.Fatalf("delete sent %d times", remote.calls)
	}
}

func TestCompensationDoesNotUndoLaterWriter(t *testing.T) {
	ctx := context.Background()
	h, err := hold(ctx, t.TempDir(), "iac", "first", lease.Of("iac/vm"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.release()
	if err := h.log.Begin(ctx, "iac/vm", []byte(`{"size":"small"}`), facade.LedgerVersionNone); err != nil {
		t.Fatal(err)
	}
	_, v, _, _ := h.log.Get(ctx, "iac/vm")
	if err := h.log.Resolve(ctx, "iac/vm", []byte("w-1"), nil, v); err != nil {
		t.Fatal(err)
	}
	_, original, _, _ := h.log.Get(ctx, "iac/vm")
	m := NewManaged("vm", []string{"create"}, "id", "id")
	run := &managedRun{name: "iac", ledger: h.log, journal: h.journal, held: h, applied: []appliedEffect{{key: "iac/vm", resource: m, address: "stackql_unstable_widgets.things.widgets", verb: "insert", identity: "w-1", version: original}}}
	if err := lease.Change(ctx, h.lease, lease.Of()); err != nil {
		t.Fatal(err)
	}
	next, err := lease.NewLeaser(h.log, nil).Acquire(ctx, "iac", lease.Of("iac/vm"), "second", leaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.log.Begin(ctx, "iac/vm", []byte(`{"size":"large"}`), original); err != nil {
		t.Fatal(err)
	}
	_, v, _, _ = h.log.Get(ctx, "iac/vm")
	if err := h.log.Resolve(ctx, "iac/vm", []byte("w-1"), nil, v); err != nil {
		t.Fatal(err)
	}
	if err := next.Release(ctx); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(204) }))
	defer server.Close()
	p := &convergeGraphPlan{registry: "testdata/authreg", args: Args{Endpoint: server.URL}}
	done, left := p.compensate(ctx, run)
	if len(done) != 0 || len(left) != 1 || requests.Load() != 0 {
		t.Fatalf("compensation=%v left=%v requests=%d", done, left, requests.Load())
	}
	entry, _, found, err := h.log.Get(ctx, "iac/vm")
	if err != nil || !found || string(entry.Plan()) != `{"size":"large"}` {
		t.Fatal("later writer was overwritten")
	}
}
