package omnisdk

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// Two values share a join key exactly when compareValues calls them equal, so a probe finds what a
// scan would have kept.
func TestJoinKeyAgreesWithCompare(t *testing.T) {
	vals := []any{nil, 0, -0.0, "0", "-0", 1, 1.0, "1", "1.0", "1e0", " 1", "01", 2, "abc", "ABC", "",
		true, "true", "NaN", "Inf", "+Inf", "-Inf", 1.5, "1.50"}
	for _, a := range vals {
		for _, b := range vals {
			c, ok := compareValues(a, b)
			eq := ok && c == 0
			ka, oka := joinKey(a)
			kb, okb := joinKey(b)
			if got := oka && okb && ka == kb; got != eq {
				t.Errorf("%#v vs %#v: keys equal = %v, compare equal = %v", a, b, got, eq)
			}
		}
	}
}

type feed struct {
	rows chan facade.Record
	cur  facade.Record
}

func (f *feed) Next(ctx context.Context) bool {
	r, ok := <-f.rows
	f.cur = r
	return ok
}
func (f *feed) Record() facade.Record { return f.cur }
func (f *feed) Err() error            { return nil }
func (f *feed) Close() error          { return nil }

func row(m map[string]any) facade.Record {
	return record.NewRecord(map[string]facade.Value{facade.AnonymousPayload: value.NewDocValue(m)})
}

// A probe streams: a matching row recorded after the reader started still reaches it, a row with
// another key never does, and the reader ends when the recording does.
func TestProbeStreamsMatchesOnly(t *testing.T) {
	src := &feed{rows: make(chan facade.Record)}
	store := &replayStore{entries: map[string]*replayEntry{}}
	e := store.entry("k", func() facade.Records { return src })
	r := &probeReader{e: e, column: "id", key: mustKey(t, 7)}

	got := make(chan string, 10)
	go func() {
		for r.Next(context.Background()) {
			m, _ := bind.DocMap(r.Record())
			got <- fmt.Sprint(m["n"])
		}
		close(got)
	}()
	src.rows <- row(map[string]any{"id": 1, "n": "a"})
	src.rows <- row(map[string]any{"id": "7", "n": "b"})
	select {
	case n := <-got:
		if n != "b" {
			t.Fatalf("first = %q, want b", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("matching row did not stream before the recording finished")
	}
	src.rows <- row(map[string]any{"id": 7.0, "n": "c"})
	src.rows <- row(map[string]any{"n": "no id"})
	close(src.rows)
	var rest []string
	for n := range got {
		rest = append(rest, n)
	}
	if fmt.Sprint(rest) != "[c]" {
		t.Errorf("rest = %v, want [c]", rest)
	}
}

func mustKey(t *testing.T, v any) string {
	t.Helper()
	k, ok := joinKey(v)
	if !ok {
		t.Fatalf("no key for %v", v)
	}
	return k
}
