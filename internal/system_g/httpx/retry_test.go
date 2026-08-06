package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/admit"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/retry"
)

// Admission caps concurrent requests to one backend: PerScope(2) against a slow server must never
// let more than 2 be in flight at once, even with many callers (all sharing the host scope).
func TestHTTPXAdmissionCapsConcurrency(t *testing.T) {
	const limit = 2
	var cur, max int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c := atomic.AddInt32(&cur, 1)
		for {
			m := atomic.LoadInt32(&max)
			if c <= m || atomic.CompareAndSwapInt32(&max, m, c) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	ctx := admit.WithAdmissions(context.Background(), admit.PerScope(limit))
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs := Make(Request{Method: http.MethodGet, URL: srv.URL}, nil)(nil).Open(ctx)
			defer rs.Close()
			for rs.Next(ctx) { //nolint // drive
			}
		}()
	}
	wg.Wait()
	if max > limit {
		t.Errorf("observed %d concurrent requests, want <= %d (admission breached)", max, limit)
	}
	if max < 2 {
		t.Errorf("observed max concurrency %d — admission likely serialised everything by accident", max)
	}
}

// collect opens op with the given ctx (so a retry policy can be carried on it) and returns the
// AnonymousPayload of each emitted record.
func collect(t *testing.T, ctx context.Context, op facade.Operator) []string {
	t.Helper()
	rs := op.Open(ctx)
	defer rs.Close()
	var out []string
	for rs.Next(ctx) {
		v := rs.Record().Get(facade.AnonymousPayload)
		b, _ := io.ReadAll(v.Reader())
		out = append(out, string(b))
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}
	return out
}

// A flaky endpoint (503 twice, then 200) must be recovered by the run's retry policy: httpx
// reattempts on the ephemeral status and emits the eventual success.
func TestHTTPXRetriesEphemeral(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) <= 2 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	ctx := retry.WithPolicy(context.Background(), retry.New(retry.Config{
		Tries: 5, Backoff: retry.NewFullJitter(time.Millisecond, time.Second),
	}))
	bodies := collect(t, ctx, Make(Request{Method: http.MethodGet, URL: srv.URL}, nil)(nil))

	if len(bodies) != 1 || bodies[0] != "ok" {
		t.Fatalf("bodies = %v, want one \"ok\"", bodies)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("server hit %d times, want 3 (two 503s then 200)", got)
	}
}

// A permanent status (404) is NOT retried and is handed downstream unchanged.
func TestHTTPXDoesNotRetryPermanent(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	ctx := retry.WithPolicy(context.Background(), retry.New(retry.Config{Tries: 5, Backoff: retry.NewFullJitter(time.Millisecond, time.Second)}))
	_ = collect(t, ctx, Make(Request{Method: http.MethodGet, URL: srv.URL}, nil)(nil))

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server hit %d times, want 1 (404 is permanent, no retry)", got)
	}
}

// Many concurrent requests share ONE policy + governor against a flaky server; all must recover.
// The shared rate governor staggers the retries (run with -race for concurrency safety).
func TestHTTPXConcurrentSharedPolicyRecovers(t *testing.T) {
	// Each distinct path fails once (503) then succeeds, so every caller must retry exactly once.
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path]++
		n := seen[r.URL.Path]
		mu.Unlock()
		if n == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// One shared policy for all callers — aggregate governance under parallelism.
	ctx := retry.WithPolicy(context.Background(), retry.New(retry.Config{
		Tries: 5, Backoff: retry.NewFullJitter(time.Millisecond, time.Second), Governor: retry.NewRateGovernor(1000, 16),
	}))

	const n = 30
	var wg sync.WaitGroup
	var failed int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := Request{Method: http.MethodGet, URL: srv.URL + "/" + strconv.Itoa(i)}
			rs := Make(req, nil)(nil).Open(ctx)
			defer rs.Close()
			var body string
			for rs.Next(ctx) {
				v := rs.Record().Get(facade.AnonymousPayload)
				b, _ := io.ReadAll(v.Reader())
				body = string(b)
			}
			if rs.Err() != nil || body != "ok" {
				atomic.AddInt32(&failed, 1)
			}
		}(i)
	}
	wg.Wait()
	if failed != 0 {
		t.Errorf("%d/%d concurrent callers failed to recover", failed, n)
	}
}
