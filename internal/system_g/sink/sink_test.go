package sink

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

// lockedBuf is a concurrency-safe bytes.Buffer so the test's assertions on the destination are
// themselves race-free (the sink serialises writes, but the test reads under -race too).
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Many goroutines writing whole records concurrently must all land, none torn, and Close flushes.
func TestAsyncConcurrentWholeRecords(t *testing.T) {
	dst := &lockedBuf{}
	w := NewAsync(dst, 8)

	const producers, each = 20, 50
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			line := strings.Repeat(string(rune('a'+p)), 40) + "\n" // a distinct, whole line per producer
			for i := 0; i < each; i++ {
				if _, err := w.Write([]byte(line)); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(p)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Every line must be intact (all same char) and the total count exact.
	sc := bufio.NewScanner(strings.NewReader(dst.String()))
	var lines int
	for sc.Scan() {
		l := sc.Text()
		if len(l) != 40 {
			t.Fatalf("torn line (len %d): %q", len(l), l)
		}
		for i := 1; i < len(l); i++ {
			if l[i] != l[0] {
				t.Fatalf("interleaved line: %q", l)
			}
		}
		lines++
	}
	if lines != producers*each {
		t.Errorf("got %d lines, want %d", lines, producers*each)
	}
}

type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }

// A destination write error is sticky and surfaces at Close.
func TestAsyncSurfacesWriteError(t *testing.T) {
	boom := errors.New("disk full")
	w := NewAsync(errWriter{err: boom}, 4)
	_, _ = w.Write([]byte("x")) // enqueued; the drain goroutine will fail on it
	if err := w.Close(); !errors.Is(err, boom) {
		t.Errorf("Close err = %v, want %v", err, boom)
	}
}

// Close is idempotent.
func TestAsyncCloseIdempotent(t *testing.T) {
	w := NewAsync(&lockedBuf{}, 2)
	if err := w.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// A Write after Close must be dropped with ErrClosed, never panic (producers — e.g. a cancelled
// request still logging — can outlive Close on the error path).
func TestAsyncWriteAfterCloseDoesNotPanic(t *testing.T) {
	w := NewAsync(&lockedBuf{}, 2)
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := w.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close err = %v, want ErrClosed", err)
	}
}

// Writes racing Close (still-live producers) must never panic on a closed channel — the whole point
// of the closing-signal design. Some land, some drop; none crash.
func TestAsyncWriteRacingCloseNeverPanics(t *testing.T) {
	w := NewAsync(&lockedBuf{}, 8)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = w.Write([]byte("rec\n")) // ErrClosed after close is fine; a panic is not
			}
		}()
	}
	_ = w.Close() // races the writers above
	wg.Wait()     // if any writer panicked, the test crashes
}
