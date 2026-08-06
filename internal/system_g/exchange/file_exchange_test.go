package exchange

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

func TestFileExchangeRead(t *testing.T) {
	const path = "testdata/test_file.md"
	e := NewFileExchange(
		0,
		path,
		"r",
		facade.FormRead,
	)

	ctx := context.Background()
	rs := e.Open(ctx)
	defer rs.Close()

	var got []string
	for rs.Next(ctx) {
		v := rs.Record().Get(facade.AnonymousPayload)
		if v == nil {
			t.Fatal("record missing anonymous payload")
		}
		b, err := io.ReadAll(v.Reader())
		if err != nil {
			t.Fatalf("read value: %v", err)
		}
		got = append(got, string(b))
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("stream err: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	want := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")

	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q want %q", i, got[i], want[i])
		}
	}
}
