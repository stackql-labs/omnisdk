package exchange

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

// wordCountTransform counts the words in each incoming page's anonymous payload and
// emits a record with that count under "words".
type wordCountTransform struct{}

func (t wordCountTransform) Apply(in facade.Page) (facade.Record, error) {
	b := in.Bytes(facade.AnonymousPayload)
	count := len(strings.Fields(string(b))) // empty line → 0 words, still one record
	return record.NewRecord(map[string]facade.Value{
		"words": value.NewBytesValue([]byte(strconv.Itoa(count))),
	}), nil
}

// file read → per-record word-count transform. Each output record's "words" must equal
// the word count of the corresponding file line.
func TestFileExchangeWordCountTransform(t *testing.T) {
	const path = "testdata/test_file.md"

	file := NewFileExchange(0, path, "r", facade.FormRead)
	xform := NewTransformExchange(1, file, wordCountTransform{}, 1)

	ctx := context.Background()
	rs := xform.Open(ctx)
	defer rs.Close()

	var got []int
	for rs.Next(ctx) {
		v := rs.Record().Get("words")
		if v == nil {
			t.Fatal("record missing 'words'")
		}
		b, err := io.ReadAll(v.Reader())
		if err != nil {
			t.Fatalf("read value: %v", err)
		}
		n, err := strconv.Atoi(string(b))
		if err != nil {
			t.Fatalf("parse count %q: %v", b, err)
		}
		got = append(got, n)
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")

	if len(got) != len(lines) {
		t.Fatalf("got %d records, want %d", len(got), len(lines))
	}
	for i, line := range lines {
		want := len(strings.Fields(line))
		if got[i] != want {
			t.Errorf("line %d: got %d words, want %d", i, got[i], want)
		}
	}
}
