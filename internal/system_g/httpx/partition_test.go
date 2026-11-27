package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/retry"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

type partitionTransport func(*http.Request) (*http.Response, error)

func (f partitionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func partitionResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}
}

func partitionContext(t *testing.T, transport partitionTransport, tries int) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	ctx = WithClient(ctx, &http.Client{Transport: transport})
	return retry.WithPolicy(ctx, retry.New(retry.Config{
		Tries: tries, Backoff: retry.NewFullJitter(time.Millisecond, time.Millisecond),
	}))
}

func TestHTTPXDetailFanoutDuringNetworkPartition(t *testing.T) {
	const details, tries = 12, 3
	var disconnected atomic.Bool
	disconnected.Store(true)
	var attempts [details]atomic.Int32
	ctx := partitionContext(t, func(req *http.Request) (*http.Response, error) {
		id, err := strconv.Atoi(req.URL.Query().Get("id"))
		if err != nil || id < 0 || id >= details {
			return nil, errors.New("unexpected detail request")
		}
		attempts[id].Add(1)
		if disconnected.Load() {
			return nil, syscall.ENETUNREACH
		}
		return partitionResponse(req.URL.Query().Get("id")), nil
	}, tries)

	var wg sync.WaitGroup
	for id := range details {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			op := Make(Request{Method: http.MethodGet, URL: "https://bucket.test/detail",
				Query: map[string]string{"id": strconv.Itoa(id)}}, nil)(nil)
			rows := op.Open(ctx)
			defer rows.Close()
			if rows.Next(ctx) || !errors.Is(rows.Err(), syscall.ENETUNREACH) {
				t.Errorf("detail %d: expected a network error, got %v", id, rows.Err())
			}
			if got := attempts[id].Load(); got != tries {
				t.Errorf("detail %d: got %d attempts, want %d", id, got, tries)
			}
		}(id)
	}
	wg.Wait()

	disconnected.Store(false)
	for id := range details {
		want := strconv.Itoa(id)
		op := Make(Request{Method: http.MethodGet, URL: "https://bucket.test/detail",
			Query: map[string]string{"id": want}}, nil)(nil)
		if got := collect(t, ctx, op); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("detail %d after reconnect: got %v", id, got)
		}
		if got := attempts[id].Load(); got != tries+1 {
			t.Errorf("detail %d after reconnect: got %d total attempts, want %d", id, got, tries+1)
		}
	}
}

func TestHTTPXPageRetryAfterNetworkPartition(t *testing.T) {
	var tokens []string
	ctx := partitionContext(t, func(req *http.Request) (*http.Response, error) {
		token := req.URL.Query().Get("page")
		tokens = append(tokens, token)
		switch token {
		case "":
			return partitionResponse(`{"id":1,"next":"p2"}`), nil
		case "p2":
			if len(tokens) <= 3 {
				return nil, syscall.ENETUNREACH
			}
			return partitionResponse(`{"id":2,"next":"p3"}`), nil
		case "p3":
			return partitionResponse(`{"id":3}`), nil
		default:
			return nil, errors.New("unexpected page token")
		}
	}, 3)

	op := Make(Request{Method: http.MethodGet, URL: "https://query.test/rows",
		Continuation: Continuation{Kind: ContPaginate, NextTokenPath: "next", TokenParam: "page"}}, nil)(nil)
	// Only the failed page should be retried, and each successful page should be emitted once.
	observed := collect(t, ctx, op)
	if want := []string{`{"id":1,"next":"p2"}`, `{"id":2,"next":"p3"}`, `{"id":3}`}; !reflect.DeepEqual(observed, want) {
		t.Errorf("pages = %v, want %v", observed, want)
	}
	if want := []string{"", "p2", "p2", "p2", "p3"}; !reflect.DeepEqual(tokens, want) {
		t.Errorf("requested tokens = %v, want %v", tokens, want)
	}
}

// Model a reader that persists its next page token after writing a complete page.
func TestHTTPXBigQueryJSONLResumeFromSavedToken(t *testing.T) {
	const tries = 3
	dir := t.TempDir()
	output, checkpoint := filepath.Join(dir, "rows.jsonl"), filepath.Join(dir, "page-token")
	var tokens []string
	disconnected := true
	transport := func(req *http.Request) (*http.Response, error) {
		token := req.URL.Query().Get("pageToken")
		tokens = append(tokens, token)
		switch token {
		case "":
			return partitionResponse(`{"rows":[{"f":[{"v":"a"}]},{"f":[{"v":"b"}]}],"pageToken":"p2"}`), nil
		case "p2":
			if disconnected {
				return nil, syscall.ENETUNREACH
			}
			return partitionResponse(`{"rows":[{"f":[{"v":"c"}]}]}`), nil
		default:
			return nil, errors.New("unexpected BigQuery page token")
		}
	}

	run := func(token string) error {
		ctx := partitionContext(t, transport, tries)
		req := Request{Method: http.MethodGet,
			URL:          "https://bigquery.test/bigquery/v2/projects/p/datasets/d/tables/t/data",
			Query:        map[string]string{"pageToken": token},
			Continuation: Continuation{Kind: ContPaginate, NextTokenPath: "pageToken", TokenParam: "pageToken"}}
		rows := Make(req, nil)(nil).Open(ctx)
		defer rows.Close()
		file, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		enc := encoder.NewJSONLEncoder()
		for rows.Next(ctx) {
			var page struct {
				Rows      []any  `json:"rows"`
				PageToken string `json:"pageToken"`
			}
			payload := rows.Record().Get(facade.AnonymousPayload)
			if err := json.NewDecoder(payload.Reader()).Decode(&page); err != nil {
				t.Fatal(err)
			}
			for _, row := range page.Rows {
				rec := record.NewRecord(map[string]facade.Value{
					facade.AnonymousPayload: value.NewDocValue([]any{row}),
				})
				if _, err := enc.Encode(file, rec); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(checkpoint, []byte(page.PageToken), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return rows.Err()
	}

	if err := run(""); !errors.Is(err, syscall.ENETUNREACH) {
		t.Fatalf("first run error = %v, want network unreachable", err)
	}
	saved, err := os.ReadFile(checkpoint)
	if err != nil || string(saved) != "p2" {
		t.Fatalf("saved checkpoint = %q, err = %v", saved, err)
	}
	disconnected = false
	if err := run(string(saved)); err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if want := []string{"", "p2", "p2", "p2", "p2"}; !reflect.DeepEqual(tokens, want) {
		t.Errorf("requested tokens = %v, want %v", tokens, want)
	}
	got, err := os.ReadFile(output)
	want := "{\"f\":[{\"v\":\"a\"}]}\n{\"f\":[{\"v\":\"b\"}]}\n{\"f\":[{\"v\":\"c\"}]}\n"
	if err != nil || string(got) != want {
		t.Fatalf("JSONL = %q, err = %v, want %q", got, err, want)
	}
}
