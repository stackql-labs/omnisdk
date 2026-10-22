package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestTimeoutCoversHeadersAndBody(t *testing.T) {
	for _, body := range []bool{false, true} {
		for _, sendNode := range []bool{false, true} {
			t.Run(fmtTimeoutCase(body, sendNode), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if body {
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
				}))
				defer srv.Close()
				ctx := WithRequestTimeout(context.Background(), 50*time.Millisecond)
				// A safety deadline also makes regressions finish without hanging the suite.
				ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				rec := NewRequestRecord(http.MethodGet, srv.URL, nil, nil)
				start := time.Now()
				var err error
				if sendNode {
					_, _, err = (&sendExchange{}).send(ctx, rec)
				} else {
					_, _, _, err = (&op{}).sendOnce(ctx, rec)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("error = %v, want deadline exceeded", err)
				}
				if elapsed := time.Since(start); elapsed > time.Second {
					t.Fatalf("request took %s; per-request deadline was not applied", elapsed)
				}
			})
		}
	}
}

func fmtTimeoutCase(body, sendNode bool) string {
	name := "headers"
	if body {
		name = "body"
	}
	if sendNode {
		return name + "/send"
	}
	return name + "/inline"
}

func TestRequestTimeoutPreservesClientOverrideAndParentDeadline(t *testing.T) {
	base := WithRequestTimeout(context.Background(), time.Millisecond)
	client := &http.Client{Timeout: time.Hour}
	ctx, cancel := requestContext(base, client)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) < 59*time.Minute {
		t.Fatal("explicit client timeout was not preserved")
	}
	parent, stop := context.WithTimeout(base, 10*time.Millisecond)
	defer stop()
	ctx, cancel = requestContext(parent, client)
	defer cancel()
	if deadline, _ := ctx.Deadline(); deadline != mustDeadline(parent) {
		t.Fatal("client override extended parent deadline")
	}
	ctx, cancel = requestContext(context.Background(), http.DefaultClient)
	defer cancel()
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 29*time.Second || time.Until(deadline) > DefaultRequestTimeout {
		t.Fatal("unset timeout should use the finite default")
	}
	ctx, cancel = requestContext(WithRequestTimeout(context.Background(), -1), http.DefaultClient)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("negative timeout should explicitly disable the deadline")
	}
}

func mustDeadline(ctx context.Context) time.Time {
	d, _ := ctx.Deadline()
	return d
}
