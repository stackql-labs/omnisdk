package buffer_test

import (
	"context"
	"io"
	"strconv"
	"sync"
	"testing"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/record"
)

// tv is a stub facade.Value for tests.
type tv struct{}

func (tv) WriteTo(io.Writer) (int64, error) { return 0, nil }
func (tv) Read(io.Reader)                   {}
func (tv) Type() facade.Type                { return nil }
func (tv) Reader() io.Reader                { return nil }

// One producer, two consumers, capacity smaller than N so back-pressure engages.
// Each consumer must see all N records in order; reclaim must keep memory bounded.
func TestBufferFanout(t *testing.T) {
	const (
		n        = 2000
		readers  = 2
		chunkSz  = 8
		capacity = 16
	)
	b := buffer.NewBuffer(readers, chunkSz, capacity)
	ctx := context.Background()

	var wg sync.WaitGroup

	consume := func() {
		defer wg.Done()
		r := b.Reader()
		defer r.Close()
		i := 0
		for r.Next(ctx) {
			if v := r.Record().Get(strconv.Itoa(i)); v == nil {
				t.Errorf("record %d: expected key %d present", i, i)
			}
			i++
		}
		if err := r.Err(); err != nil {
			t.Errorf("consumer Err: %v", err)
		}
		if i != n {
			t.Errorf("consumer saw %d records, want %d", i, n)
		}
	}

	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go consume()
	}

	go func() {
		for i := 0; i < n; i++ {
			rec := record.NewRecord(map[string]facade.Value{strconv.Itoa(i): tv{}})
			if err := b.Append(ctx, rec); err != nil {
				t.Errorf("append %d: %v", i, err)
				return
			}
		}
		b.Complete(nil)
	}()

	wg.Wait()
}
