package omnisdk_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/pkg/omnisdk"
)

// s3Store is a bucket and its tags; refuse makes the bucket's delete fail as a non-empty bucket does.
type s3Store struct {
	mu      sync.Mutex
	c       wireLog
	bucket  bool
	tagging string
	refuse  bool
}

func (s *s3Store) serve(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		_, tagged := r.URL.Query()["tagging"]
		b, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPut && tagged:
			s.c.add("put tagging")
			s.tagging = string(b)
		case r.Method == http.MethodPut && r.URL.Path == "/omni-demo":
			s.c.add("create")
			s.bucket = true
		case r.Method == http.MethodGet && r.URL.Path == "/":
			fmt.Fprint(w, `<ListAllMyBucketsResult><Buckets>`)
			if s.bucket {
				fmt.Fprint(w, `<Bucket><Name>omni-demo</Name></Bucket>`)
			}
			fmt.Fprint(w, `</Buckets></ListAllMyBucketsResult>`)
		case r.Method == http.MethodGet && tagged:
			fmt.Fprint(w, s.tagging)
		case r.Method == http.MethodDelete && tagged:
			s.c.add("delete tagging")
			s.tagging = ""
		case r.Method == http.MethodDelete && r.URL.Path == "/omni-demo":
			s.c.add("delete bucket")
			if s.refuse {
				http.Error(w, `<Error><Code>BucketNotEmpty</Code></Error>`, http.StatusConflict)
				return
			}
			s.bucket = false
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusNotFound)
		}
	}))
}

func s3Resources() []omnisdk.ManagedResource {
	return []omnisdk.ManagedResource{
		omnisdk.NewResource("aws/s3/bucket", "aws", "s3.buckets", []byte(`{}`),
			map[string]string{"bucket": "omni-demo"}, nil, "", "", "Name", "bucket", ""),
		omnisdk.NewResource("aws/s3/bucket-tags", "aws", "s3.bucket_taggings",
			[]byte(`{"Tagging":{"TagSet":{"Tag":[{"Key":"env","Value":"dev"}]}}}`),
			nil, []omnisdk.Arrival{{From: "aws/s3/bucket", As: "bucket"}}, "", "", "", "bucket", ""),
	}
}

func destroy(t *testing.T, state, runID string, args omnisdk.Args) []omnisdk.Row {
	t.Helper()
	pl, err := omnisdk.Prepare(corpus, omnisdk.Destroy(), omnisdk.NewLedger("demo", state, runID), args)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return drain(t, pl)
}

// Destroy removes what the collection made from the ledger alone: the tags first, since they depend
// on the bucket, then the bucket. A second destroy finds nothing to do.
func TestDestroyRemovesDependentsFirst(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	s := &s3Store{}
	srv := s.serve(t)
	defer srv.Close()
	args := omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"region": "us-east-1"}}
	state := t.TempDir()
	converge(t, corpus, state, "r1", args, s3Resources()...)
	s.c.took()

	rows := destroy(t, state, "d1", args)
	if got := s.c.took(); got != "delete tagging, delete bucket" {
		t.Errorf("calls = %s, want the tags removed before the bucket", got)
	}
	for _, r := range rows {
		if r["status"] != "destroyed" {
			t.Errorf("row %v, want destroyed", r)
		}
	}
	if len(rows) != 2 || s.bucket {
		t.Errorf("rows = %v, bucket left = %v", rows, s.bucket)
	}
	if again := destroy(t, state, "d2", args); len(again) != 0 || s.c.took() != "" {
		t.Errorf("second destroy = %v, want nothing", again)
	}
}

// A delete the provider refuses is retried while other deletes make progress, and what still
// refuses is reported with the provider's reason, kept in the ledger, never forced.
func TestDestroyReportsWhatItCannotRemove(t *testing.T) {
	requireCorpus(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	s := &s3Store{}
	srv := s.serve(t)
	defer srv.Close()
	args := omnisdk.Args{Endpoint: srv.URL, Params: map[string]string{"region": "us-east-1"}}
	state := t.TempDir()
	converge(t, corpus, state, "r1", args, s3Resources()...)
	s.refuse = true

	rows := destroy(t, state, "d1", args)
	byKey := map[string]omnisdk.Row{}
	for _, r := range rows {
		byKey[fmt.Sprint(r["key"])] = r
	}
	if byKey["demo/aws/s3/bucket-tags"]["status"] != "destroyed" {
		t.Errorf("tags: %v, want destroyed", byKey["demo/aws/s3/bucket-tags"])
	}
	bucket := byKey["demo/aws/s3/bucket"]
	if bucket["status"] != "outstanding" || !strings.Contains(fmt.Sprint(bucket["reason"]), "BucketNotEmpty") {
		t.Errorf("bucket: %v, want outstanding with the provider's reason", bucket)
	}
	s.refuse = false
	if again := destroy(t, state, "d2", args); len(again) != 1 || again[0]["status"] != "destroyed" {
		t.Errorf("after the refusal clears: %v, want the bucket destroyed", again)
	}
}

// A collection made by a hand-written graph is destroyed the same way: its mutations recorded what
// each resource is when they landed.
func TestDestroyAHandWrittenGraphsCollection(t *testing.T) {
	store := &widgetStore{items: map[string]map[string]any{}}
	srv := store.serve(t)
	defer srv.Close()
	state := t.TempDir()
	convergeWidgets(t, srv, state, map[string]map[string]any{
		"w1": {"size": "L", "color": "red"}, "w2": {"size": "M", "color": "blue"},
	}, []string{"w1", "w2"})
	if len(store.items) != 2 {
		t.Fatalf("store = %v, want two widgets", store.items)
	}
	store.took()

	pl, err := omnisdk.Prepare(authRegistry, omnisdk.Destroy(), omnisdk.NewLedger("demo", state, "d1"), omnisdk.Args{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	rows := drain(t, pl)
	for _, r := range rows {
		if r["status"] != "destroyed" {
			t.Errorf("row %v, want destroyed", r)
		}
	}
	if len(rows) != 2 || len(store.items) != 0 {
		t.Errorf("rows = %v, store = %v, want both destroyed", rows, store.items)
	}
}
