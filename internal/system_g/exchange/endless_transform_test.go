package exchange

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
	"github.com/stackql-labs/omnisdk/internal/system_g/value"
)

var (
	_ facade.Operator = &endlessExchange{}
)

// endlessExchange is a never-ending producer of "hello" records. Its goroutine only
// stops when every reader has Closed (ErrAllReadersClosed). done closes when it exits,
// so a test can assert input cleanup.
type endlessExchange struct {
	done chan struct{}
}

func newEndlessExchange() *endlessExchange {
	return &endlessExchange{done: make(chan struct{})}
}

func (e *endlessExchange) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 16, 4) // bounded so the endless producer can't run away
	go func() {
		defer close(e.done)
		defer buf.Complete(nil)
		for {
			rec := record.NewRecord(map[string]facade.Value{
				facade.AnonymousPayload: value.NewBytesValue([]byte("hello")),
			})
			if err := buf.Append(ctx, rec); err != nil {
				return // ErrAllReadersClosed or ctx cancel → stop producing
			}
		}
	}()
	return buf.Reader()
}

// passthrough emits each input page's payload unchanged, as a fresh record.
type passthrough struct{}

func (p passthrough) Apply(in facade.Page) (facade.Record, error) {
	return record.NewRecord(map[string]facade.Value{
		facade.AnonymousPayload: value.NewBytesValue(in.Bytes(facade.AnonymousPayload)),
	}), nil
}

// An endless upstream feeds a transform; the consumer takes exactly 4 records then
// closes ("exit success"). Verify output length, content, and that the upstream
// producer goroutine is cleaned up as a result of the close.
func TestEndlessUpstreamTakeFour(t *testing.T) {
	up := newEndlessExchange()
	xform := NewTransformExchange(1, up, passthrough{}, 1)

	ctx := context.Background()
	rs := xform.Open(ctx)

	var got []string
	for i := 0; i < 4; i++ {
		if !rs.Next(ctx) {
			t.Fatalf("stream ended early at %d: err=%v", i, rs.Err())
		}
		v := rs.Record().Get(facade.AnonymousPayload)
		if v == nil {
			t.Fatalf("record %d missing payload", i)
		}
		b, err := io.ReadAll(v.Reader())
		if err != nil {
			t.Fatalf("read value: %v", err)
		}
		got = append(got, string(b))
	}

	// exit success after 4: close the downstream cursor.
	if err := rs.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// length + content
	if len(got) != 4 {
		t.Fatalf("got %d records, want 4", len(got))
	}
	for i, s := range got {
		if s != "hello" {
			t.Errorf("record %d = %q, want %q", i, s, "hello")
		}
	}

	// input cleanup: the close must propagate up the pipeline and stop the endless
	// producer goroutine (transform closes its upstream on exit).
	select {
	case <-up.done:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream producer did not clean up after downstream close")
	}
}
