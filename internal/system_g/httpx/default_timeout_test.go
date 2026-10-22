package httpx

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/retry"
)

type stalledTransport struct{ calls int }

func (s *stalledTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.calls++
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestDefaultTimeoutPrecedesProcessGuard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		ctx = retry.WithPolicy(ctx, retry.New(retry.Config{Tries: 4}))
		transport := &stalledTransport{}
		o := &op{client: &http.Client{Transport: transport}}
		start := time.Now()
		_, _, err := o.send(ctx, NewRequestRecord(http.MethodGet, "http://example.com", nil, nil))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error=%v, want deadline exceeded", err)
		}
		if elapsed := time.Since(start); elapsed != DefaultRequestTimeout {
			t.Fatalf("elapsed=%s, want %s", elapsed, DefaultRequestTimeout)
		}
		if transport.calls != 1 {
			t.Fatalf("attempts=%d, want 1", transport.calls)
		}
		if ctx.Err() != nil {
			t.Fatal("process guard fired before the request timeout")
		}
	})
}
