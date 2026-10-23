package docx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/httpx"
)

type graphSpecTest struct{ strategy, terminate string }

func (graphSpecTest) URL() string      { return "" }
func (graphSpecTest) HTTPVerb() string { return "POST" }
func (graphSpecTest) Query() string {
	return "query { things(first: {{ .limit }}{{ .cursor }}) {nodes{name}} }"
}
func (graphSpecTest) Selection() string         { return "$.data.nodes[*]" }
func (graphSpecTest) CursorPath() string        { return "$.data.cursor" }
func (g graphSpecTest) CursorStrategy() string  { return g.strategy }
func (graphSpecTest) CursorFormat() string      { return "" }
func (g graphSpecTest) TerminationPath() string { return g.terminate }
func (graphSpecTest) PageSize() int             { return 0 }

func TestGraphQLCursorCannotLoopAndQueryBracesRemainLiteral(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !strings.Contains(body["query"], "first: 42") || !strings.Contains(body["query"], "nodes{name}") {
			t.Errorf("query was not rendered literally: %s", body["query"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"nodes":[{"name":"one"}],"cursor":"same"}}`)
	}))
	defer srv.Close()
	op := newGraphQLOperator(graphSpecTest{}, "", httpx.Request{URL: srv.URL}, nil, nil, 42, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rows := op.Open(ctx)
	defer rows.Close()
	for rows.Next(ctx) {
	}
	if rows.Err() == nil || !strings.Contains(rows.Err().Error(), "cursor did not advance") {
		t.Fatalf("unbounded cursor: %v", rows.Err())
	}
	if calls.Load() != 2 {
		t.Fatalf("requests = %d", calls.Load())
	}
}

func TestGraphQLPageInfoCannotSilentlyDropMissingCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"nodes":[{"name":"one"}],"more":true}}`)
	}))
	defer srv.Close()
	op := newGraphQLOperator(graphSpecTest{strategy: "page_info", terminate: "$.data.more"}, "", httpx.Request{URL: srv.URL}, nil, nil, 10, "")
	rows := op.Open(context.Background())
	defer rows.Close()
	for rows.Next(context.Background()) {
	}
	if rows.Err() == nil || !strings.Contains(rows.Err().Error(), "cursor is missing") {
		t.Fatalf("pagination truncated silently: %v", rows.Err())
	}
}

func TestGraphQLHTTPDeadlineStillCoversBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx := httpx.WithRequestTimeout(context.Background(), 30*time.Millisecond)
	op := newGraphQLOperator(graphSpecTest{}, "", httpx.Request{URL: srv.URL}, nil, nil, 10, "")
	rows := op.Open(ctx)
	defer rows.Close()
	start := time.Now()
	if rows.Next(ctx) || rows.Err() == nil {
		t.Fatalf("stalled body did not fail: %v", rows.Err())
	}
	if time.Since(start) > time.Second {
		t.Fatal("GraphQL bypassed the request timeout")
	}
}

func TestGraphQLPageLimitStopsWithoutError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"nodes":[{"name":"row"}],"cursor":"page-%d"}}`, page)
	}))
	defer srv.Close()
	ctx := httpx.WithPageLimit(context.Background(), 2)
	op := newGraphQLOperator(graphSpecTest{}, "", httpx.Request{URL: srv.URL}, nil, nil, 10, "")
	rows := op.Open(ctx)
	defer rows.Close()
	count := 0
	for rows.Next(ctx) {
		count++
	}
	if rows.Err() != nil || calls.Load() != 2 || count != 2 {
		t.Fatalf("requests=%d rows=%d error=%v", calls.Load(), count, rows.Err())
	}
}
