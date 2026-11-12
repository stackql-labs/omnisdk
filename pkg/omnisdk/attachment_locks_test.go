package omnisdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Two attachment mutations consume the same subnet. They may overlap, but the
// subnet cannot be mutated until both attachments finish. Later VM observation
// keeps only the VM guard, not the subnet. The API uses the widget fixture.
func TestConcurrentAttachmentsReleaseSubnetBeforeVMReadiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	log, err := ledger.NewFile(filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"subnet", "vm-a", "vm-b"} {
		k := facade.LedgerKey("demo/" + key)
		if err := log.Begin(ctx, k, []byte(`{"size":"old"}`), facade.LedgerVersionNone); err != nil {
			t.Fatal(err)
		}
		_, v, _, _ := log.Get(ctx, k)
		if err := log.Resolve(ctx, k, []byte(key), nil, v); err != nil {
			t.Fatal(err)
		}
	}
	attachments := make(chan struct{}, 2)
	observations := make(chan struct{}, 2)
	allowAttach := make(chan struct{})
	allowReady := make(chan struct{})
	defer close(allowReady)
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			attachments <- struct{}{}
			select {
			case <-allowAttach:
			case <-ctx.Done():
				return
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			if body["color"] != "subnet" {
				t.Errorf("lost subnet binding: %v", body)
			}
			fmt.Fprintf(w, `{"id":%q}`, strings.TrimPrefix(r.URL.Path, "/widgets/"))
		} else if r.Method == http.MethodGet {
			observations <- struct{}{}
			select {
			case <-allowReady:
			case <-ctx.Done():
				return
			}
			fmt.Fprintf(w, `[{"id":%q,"size":"ready"}]`, r.URL.Query().Get("id"))
		} else {
			t.Errorf("unexpected mutation %s", r.Method)
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	results := make(chan error, 2)
	for _, key := range []string{"vm-a", "vm-b"} {
		go func() {
			graph, err := NewGraph([]Node{
				NewRecallNode("subnet", "subnet"),
				NewMutationNode("attach", "stackql_unstable_widgets.things.widgets", "update", map[string]string{"id": key}, nil, map[string]any{"size": "attached", "color": nil}),
				NewNode("ready", "stackql_unstable_widgets.things.widgets", nil),
			}, []Wiring{
				NewWiring("attach", []Inbound{NewInbound("subnet", "identity", "color")}, "", ""),
				NewWiring("ready", []Inbound{NewInbound("attach", "id", "id")}, "", ""),
			})
			if err != nil {
				results <- err
				return
			}
			p, err := ConvergeGraph("testdata/authreg", "demo", root, "run-"+key, graph, []Managed{NewManaged(key, []string{"attach"}, "id", "id")}, Args{Endpoint: server.URL})
			if err != nil {
				results <- err
				return
			}
			rows, err := p.Open(ctx)
			if err != nil {
				results <- err
				return
			}
			defer rows.Close()
			for rows.Next() {
				if row := rows.Row(); row["status"] == "failed" {
					results <- fmt.Errorf("%v", row)
					return
				}
			}
			results <- rows.Err()
		}()
	}
	waitTwo := func(ch <-chan struct{}) {
		t.Helper()
		for i := 0; i < 2; i++ {
			select {
			case <-ch:
			case <-ctx.Done():
				t.Fatal("checkpoint not reached:", ctx.Err())
			}
		}
	}
	waitTwo(attachments)
	leaser := lease.NewLeaser(log, nil)
	if _, err := leaser.Acquire(ctx, "demo", lease.Of("demo/subnet"), "subnet-writer", time.Minute); err == nil {
		t.Fatal("subnet write admitted while attaching")
	}
	close(allowAttach)
	waitTwo(observations)
	claim, err := leaser.Acquire(ctx, "demo", lease.Of("demo/subnet"), "subnet-writer", time.Minute)
	if err != nil {
		t.Fatal("subnet held during VM readiness:", err)
	}
	if err := claim.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := leaser.Acquire(ctx, "demo", lease.Of("demo/vm-a"), "vm-writer", time.Minute); err == nil {
		t.Fatal("VM write admitted during readiness observation")
	}
	if peak.Load() != 2 {
		t.Fatalf("attachments did not overlap: peak=%d", peak.Load())
	}
	// Release observation without closing twice in cleanup.
	allowReady <- struct{}{}
	allowReady <- struct{}{}
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
