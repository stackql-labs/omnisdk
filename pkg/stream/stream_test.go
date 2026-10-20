package stream_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/stream"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// gcsColumns are declared out of alphabetical order on purpose: a sink keeps the declared order.
var gcsColumns = []string{"name", "projectNumber", "labels", "versioning", "logging"}

// gcsRows is the two-bucket listing the bundled mock serves, decoded as the engine decodes a
// response: numbers kept as the text they were sent as.
func gcsRows(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "test", "mock", "collateral", "gcs", "buckets.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc struct {
		Items []map[string]any `json:"items"`
	}
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Items) != 2 {
		t.Fatalf("fixture has %d buckets, want 2", len(doc.Items))
	}
	return doc.Items
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func writeAll(t *testing.T, w stream.Writer, err error, rows []map[string]any) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGoldenJSONL(t *testing.T) {
	var b bytes.Buffer
	w, err := stream.NewJSONL(&b, gcsColumns, stream.Options{})
	writeAll(t, w, err, gcsRows(t))
	golden(t, "gcs.jsonl", b.Bytes())
}

func TestGoldenCSV(t *testing.T) {
	var b bytes.Buffer
	w, err := stream.NewDelimited(&b, gcsColumns, ',', stream.Options{})
	writeAll(t, w, err, gcsRows(t))
	golden(t, "gcs.csv", b.Bytes())
}

func TestGoldenOTel(t *testing.T) {
	var b bytes.Buffer
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	w, err := stream.NewOTelLogs(&b, gcsColumns, stream.Options{BatchSize: 2},
		stream.OTel{Scope: "omnisdk", Now: func() time.Time { return at }})
	writeAll(t, w, err, gcsRows(t))
	golden(t, "gcs.otel.jsonl", b.Bytes())
	// The project number past 2^53 is an exact OTel int, and each batch is one valid request.
	if !strings.Contains(b.String(), `{"key":"projectNumber","value":{"intValue":"9007199254740993"}}`) {
		t.Error("projectNumber not an exact intValue")
	}
	for _, line := range strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("not JSON: %s", line)
		}
	}
}

func TestGoldenTable(t *testing.T) {
	var b bytes.Buffer
	w, err := stream.NewTable(&b, gcsColumns, stream.Options{BatchSize: 10})
	writeAll(t, w, err, gcsRows(t))
	golden(t, "gcs.table.txt", b.Bytes())
}

func TestTSVAndQuoting(t *testing.T) {
	var b bytes.Buffer
	w, err := stream.NewDelimited(&b, []string{"a", "b"}, '\t', stream.Options{})
	writeAll(t, w, err, []map[string]any{{"a": "x\ty", "b": nil}, {"a": `say "hi"`, "b": 1.2e7}})
	want := "a\tb\n\"x\ty\"\t\n\"say \"\"hi\"\"\"\t12000000\n"
	if b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
	if _, err := stream.NewDelimited(&b, []string{"a"}, '"', stream.Options{}); err == nil {
		t.Error("a quote accepted as a delimiter")
	}
}

func TestEmptyRelationIsHeaded(t *testing.T) {
	var b bytes.Buffer
	w, err := stream.NewDelimited(&b, []string{"a", "b"}, ',', stream.Options{})
	writeAll(t, w, err, nil)
	if b.String() != "a,b\n" {
		t.Errorf("got %q", b.String())
	}
}

// lockedBuffer is read by the test while the flush timer writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) lines() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), "\n")
}

func TestBatchSizeFlushes(t *testing.T) {
	var b lockedBuffer
	w, _ := stream.NewJSONL(&b, []string{"n"}, stream.Options{BatchSize: 3})
	for i := range 4 {
		_ = w.Write(map[string]any{"n": i})
		if want := map[int]int{0: 0, 1: 0, 2: 3, 3: 3}[i]; b.lines() != want {
			t.Fatalf("after row %d: %d lines written, want %d", i, b.lines(), want)
		}
	}
	_ = w.Close()
	if b.lines() != 4 {
		t.Errorf("close left %d lines, want 4", b.lines())
	}
}

func TestFlushIntervalFlushes(t *testing.T) {
	var b lockedBuffer
	w, _ := stream.NewJSONL(&b, []string{"n"}, stream.Options{BatchSize: 100, FlushInterval: 10 * time.Millisecond})
	_ = w.Write(map[string]any{"n": 1})
	deadline := time.Now().Add(2 * time.Second)
	for b.lines() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.lines() != 1 {
		t.Fatal("the interval did not flush the waiting row")
	}
	_ = w.Close()
}

type failingSource struct {
	rows []map[string]any
	err  error
}

func (s *failingSource) Next() bool {
	return len(s.rows) > 0
}

func (s *failingSource) Row() map[string]any {
	r := s.rows[0]
	s.rows = s.rows[1:]
	return r
}

func (s *failingSource) Err() error { return s.err }

func TestErrorMidStreamFlushesFirst(t *testing.T) {
	var b bytes.Buffer
	w, _ := stream.NewJSONL(&b, []string{"n"}, stream.Options{BatchSize: 100})
	boom := errors.New("provider failed")
	err := stream.Copy(w, &failingSource{rows: []map[string]any{{"n": 1}, {"n": 2}}, err: boom})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the source's", err)
	}
	if b.String() != "{\"n\":1}\n{\"n\":2}\n" {
		t.Errorf("rows produced before the error were not written: %q", b.String())
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

func TestOutputFailureIsReturned(t *testing.T) {
	w, _ := stream.NewJSONL(brokenWriter{}, []string{"n"}, stream.Options{})
	if err := w.Write(map[string]any{"n": 1}); err == nil {
		t.Fatal("a failed write reported success")
	}
	if err := w.Write(map[string]any{"n": 2}); err == nil {
		t.Fatal("a sink that failed accepted another row")
	}
}

func TestNonFiniteWarnsOncePerRelation(t *testing.T) {
	var b bytes.Buffer
	w, _ := stream.NewJSONL(&b, []string{"x", "y"}, stream.Options{})
	for range 3 {
		_ = w.Write(map[string]any{"x": math.NaN(), "y": math.Inf(1)})
	}
	_ = w.Close()
	if n := len(w.Warnings()); n != 1 {
		t.Errorf("%d warnings, want 1: %v", n, w.Warnings())
	}
	if !strings.HasPrefix(b.String(), `{"x":null,"y":null}`) {
		t.Errorf("non-finite not written as null: %s", b.String())
	}
}
