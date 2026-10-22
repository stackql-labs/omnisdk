package omnisdk_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

func TestRequestTimeoutThroughPublicPlan(t *testing.T) {
	for _, tls := range []bool{false, true} {
		name := "http"
		if tls {
			name = "https"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			if tls {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			g, err := omnisdk.NewGraph([]omnisdk.Node{
				omnisdk.NewNode("n", "stackql_unstable_linked.things.items", nil),
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := omnisdk.NewGraphQuery(authRegistry, g, omnisdk.Args{
				Endpoint: srv.URL, InsecureSkipTLSVerify: tls,
				Tuning: omnisdk.Tuning{RequestTimeout: 50 * time.Millisecond},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start := time.Now()
			rows, err := plan.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				t.Error("stalled response should produce no rows")
			}
			if err := rows.Err(); err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
				t.Fatalf("error = %v, want deadline exceeded", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("request took %s; per-request deadline was not applied", elapsed)
			}
			if calls.Load() != 1 {
				t.Fatalf("deadline triggered %d attempts, want 1", calls.Load())
			}
		})
	}
}

func TestProviderTimeoutInheritanceAndIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, address, key string
		global, override   time.Duration
		wantTimeout        bool
	}{
		{"provider extends global", "linked", "linked", 20 * time.Millisecond, 500 * time.Millisecond, false},
		{"other provider inherits global", "numbered", "linked", 20 * time.Millisecond, 500 * time.Millisecond, true},
		{"namespaced provider shortens global", "linked", "stackql_unstable_linked", 500 * time.Millisecond, 20 * time.Millisecond, true},
		{"unset provider field inherits global", "linked", "linked", 500 * time.Millisecond, 0, false},
		{"provider explicitly disables timeout", "linked", "linked", 20 * time.Millisecond, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-time.After(80 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
				if tc.address == "linked" {
					_, _ = w.Write([]byte(`[{"id":"one"}]`))
				} else {
					_, _ = w.Write([]byte(`{"items":[{"id":"one"}]} `))
				}
			}))
			defer srv.Close()
			g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("n", "stackql_unstable_"+tc.address+".things.items", nil)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			pl, err := omnisdk.NewGraphQuery(authRegistry, g, omnisdk.Args{
				Endpoint: srv.URL, Tuning: omnisdk.Tuning{RequestTimeout: tc.global},
				TuningByProvider: map[string]omnisdk.ProviderTuning{tc.key: {RequestTimeout: tc.override}},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			rows, err := pl.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			n := 0
			for rows.Next() {
				n++
			}
			if tc.wantTimeout {
				if rows.Err() == nil || !strings.Contains(rows.Err().Error(), "deadline exceeded") {
					t.Fatalf("error=%v, want timeout", rows.Err())
				}
			} else if rows.Err() != nil || n != 1 {
				t.Fatalf("rows=%d error=%v, want one successful row", n, rows.Err())
			}
		})
	}
}

func TestProviderRetryOverride(t *testing.T) {
	for _, tries := range []int{0, 1} {
		t.Run(fmt.Sprintf("provider tries=%d", tries), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				_, _ = w.Write([]byte(`[{"id":"one"}]`))
			}))
			defer srv.Close()
			g, _ := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("n", "stackql_unstable_linked.things.items", nil)}, nil)
			pl, err := omnisdk.NewGraphQuery(authRegistry, g, omnisdk.Args{Endpoint: srv.URL,
				Tuning:           omnisdk.Tuning{RetryTries: 2},
				TuningByProvider: map[string]omnisdk.ProviderTuning{"linked": {RetryTries: tries}},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			rows, err := pl.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
			}
			if tries == 1 {
				if calls.Load() != 1 || rows.Err() == nil {
					t.Fatalf("calls=%d error=%v, want one failed attempt", calls.Load(), rows.Err())
				}
			} else if calls.Load() != 2 || rows.Err() != nil {
				t.Fatalf("calls=%d error=%v, want inherited two tries", calls.Load(), rows.Err())
			}
		})
	}
}

func TestProviderTimeoutIncludesAuthentication(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		if r.URL.Path != "/token" {
			t.Errorf("resource request reached before authentication: %s", r.URL.Path)
		}
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	g, err := omnisdk.NewGraph([]omnisdk.Node{omnisdk.NewNode("n", "stackql_unstable_oauthp.things.items", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphQuery(authRegistry, g, omnisdk.Args{
		Endpoint: srv.URL, Tuning: omnisdk.Tuning{RequestTimeout: 500 * time.Millisecond},
		TuningByProvider: map[string]omnisdk.ProviderTuning{"oauthp": {RequestTimeout: 20 * time.Millisecond}},
		AuthByProvider:   map[string]*omnisdk.Auth{"oauthp": {Type: "client_credentials", ClientID: "id", ClientSecret: "secret", TokenURL: srv.URL + "/token"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	rows, err := pl.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if rows.Err() == nil || !strings.Contains(rows.Err().Error(), "deadline exceeded") {
		t.Fatalf("error=%v, want authentication deadline exceeded", rows.Err())
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("authentication used the global timeout: %s", elapsed)
	}
	if calls.Load() != 1 {
		t.Fatalf("authentication made %d requests, want 1", calls.Load())
	}
}

func TestProviderConcurrencySharedAcrossExchanges(t *testing.T) {
	var active, maximum, calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`[{"id":"one"}]`))
	}))
	defer srv.Close()
	var nodes []omnisdk.Node
	for i := range 4 {
		nodes = append(nodes, omnisdk.NewNode(fmt.Sprintf("n%d", i), "stackql_unstable_linked.things.items", nil))
	}
	g, err := omnisdk.NewGraph(nodes, nil)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := omnisdk.NewGraphQuery(authRegistry, g, omnisdk.Args{Endpoint: srv.URL,
		Tuning:           omnisdk.Tuning{MaxPerHost: 4},
		TuningByProvider: map[string]omnisdk.ProviderTuning{"linked": {MaxPerHost: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := pl.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
	}
	if rows.Err() != nil || calls.Load() != 4 || maximum.Load() != 1 {
		t.Fatalf("calls=%d concurrency=%d error=%v, want four calls sharing one slot", calls.Load(), maximum.Load(), rows.Err())
	}
}
