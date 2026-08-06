// Package sink provides a concurrency-safe io.Writer for output/log destinations that many
// goroutines write to in parallel. It keeps the canonical io.Writer abstraction — no bespoke
// interface — and applies the canonical Go fan-in idiom: one goroutine owns the destination,
// producers hand it records over a bounded channel ("share memory by communicating"). The bound
// is the queue and the backpressure; a slow destination blocks writers rather than dropping.
package sink

import (
	"errors"
	"io"
	"sync"
)

// ErrClosed is returned by Write after Close (the write is dropped, not panicked on). Callers that
// ignore Write errors (e.g. fmt.Fprintf on a best-effort trace) simply lose that late record.
var ErrClosed = errors.New("sink: write after close")

// async is a concurrency-safe, backpressured io.WriteCloser. Each Write copies its bytes onto a
// bounded channel drained by a single owner goroutine, so concurrent producers never interleave
// (records land whole, in whichever order they were enqueued) and a full queue blocks Write
// instead of dropping. Write errors are reported at Close (they can't be returned synchronously)
// and are sticky, so a later Write also sees them.
//
// Whole-record integrity requires one Write per record — the caller's encoder must emit a complete
// record as a single slice (facade.Encoder does). Close drains what is already queued; a Write that
// races Close (a producer not yet quiesced, e.g. a cancelled request still logging) is dropped with
// ErrClosed rather than panicking — the data channel is never closed, only signalled.
type async struct {
	ch      chan []byte
	closing chan struct{}
	done    chan struct{}
	once    sync.Once

	mu  sync.Mutex
	err error
}

// NewAsync returns a concurrency-safe io.Writer over w with a bounded queue. queue<1 becomes 1.
func NewAsync(w io.Writer, queue int) io.WriteCloser {
	if queue < 1 {
		queue = 1
	}
	a := &async{ch: make(chan []byte, queue), closing: make(chan struct{}), done: make(chan struct{})}
	go a.run(w)
	return a
}

func (a *async) run(w io.Writer) {
	defer close(a.done)
	write := func(b []byte) {
		if a.load() != nil {
			return // drain-and-drop after the first error
		}
		if _, err := w.Write(b); err != nil {
			a.store(err)
		}
	}
	for {
		select {
		case b := <-a.ch:
			write(b)
		case <-a.closing:
			for { // flush whatever is already queued, then stop
				select {
				case b := <-a.ch:
					write(b)
				default:
					return
				}
			}
		}
	}
}

func (a *async) Write(p []byte) (int, error) {
	if err := a.load(); err != nil {
		return 0, err
	}
	select {
	case <-a.closing: // already closed → drop deterministically (the queue may still have space)
		return 0, ErrClosed
	default:
	}
	b := make([]byte, len(p)) // copy: the caller may reuse p after Write returns
	copy(b, p)
	select {
	case a.ch <- b: // blocks when the queue is full → backpressure
		return len(p), nil
	case <-a.closing: // closed while this Write blocked → drop, never panic
		return 0, ErrClosed
	}
}

// Close signals the owner to flush the queue and stop, waits for it, and returns the first write
// error. Idempotent. Writes racing Close are dropped (ErrClosed), so it is safe to call while a
// cancelled producer may still emit.
func (a *async) Close() error {
	a.once.Do(func() { close(a.closing) })
	<-a.done
	return a.load()
}

func (a *async) store(err error) {
	a.mu.Lock()
	if a.err == nil {
		a.err = err
	}
	a.mu.Unlock()
}

func (a *async) load() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}
