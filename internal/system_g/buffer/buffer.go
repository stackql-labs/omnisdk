package buffer

import (
	"context"
	"errors"
	"math"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// ErrAllReadersClosed is returned by Append once every declared reader has Closed:
// nothing consumes the buffer any more, so the producer should stop (no back-pressure
// or completion needed). Lets an unbounded buffer halt on consumer satisfaction.
var ErrAllReadersClosed = errors.New("buffer: all readers closed")

var (
	_ facade.Buffer  = &buffer{}
	_ facade.Records = &cursor{}
)

// buffer is a single-writer, multi-reader, append-only log on a β edge.
// Published records are immutable; a fixed reader set pulls via per-reader cursors.
// All coordination goes through mu + wake; records live in non-moving chunks.
type buffer struct {
	mu   sync.Mutex // guards every field below and coordinates append/read/complete
	wake sync.Cond  // parties park here; broadcast on append, reclaim, and complete

	length   int   // count of published records
	released int   // records below this index have been reclaimed (slot niled)
	done     bool  // producer reached EOF; no further appends
	err      error // terminal error (nil on clean EOF)

	// storage: chunked so existing chunks never move under readers.
	chunkSize int
	chunks    [][]facade.Record

	// back-pressure: max live (published-but-unreleased) records; 0 = unbounded.
	capacity int

	// release tracking: one cursor position per reader; min = low-water mark.
	readers  int
	assigned int   // reader ids handed out so far
	closed   int   // readers that have Closed; == readers ⇒ nobody consuming
	cursors  []int // per-reader next-index-to-read
}

// NewBuffer builds a buffer for exactly `readers` consumers. chunkSize sizes each
// storage chunk; capacity bounds live records for back-pressure (0 = unbounded).
func NewBuffer(readers, chunkSize, capacity int) facade.Buffer {
	if chunkSize < 1 {
		chunkSize = 1
	}
	b := &buffer{
		chunkSize: chunkSize,
		capacity:  capacity,
		readers:   readers,
		cursors:   make([]int, readers),
	}
	b.wake.L = &b.mu
	return b
}

func (b *buffer) Append(ctx context.Context, r facade.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Cancellation wakes any park below so we re-check ctx.
	stop := context.AfterFunc(ctx, func() {
		b.mu.Lock()
		b.wake.Broadcast()
		b.mu.Unlock()
	})
	defer stop()

	b.mu.Lock()
	defer b.mu.Unlock()

	// Wait for a free slot, EOF of consumers, or cancellation.
	for {
		if b.closed >= b.readers {
			return ErrAllReadersClosed // nobody left to consume → stop the producer
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if b.capacity <= 0 || b.length-b.released < b.capacity {
			break // slot available (or unbounded)
		}
		b.wake.Wait() // back-pressure: park until a reader advances the low-water mark
	}

	b.put(b.length, r)
	b.length++
	b.wake.Broadcast()
	return nil
}

func (b *buffer) Complete(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
	b.done = true
	b.wake.Broadcast()
}

func (b *buffer) Reader() facade.Records {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.assigned
	if id >= b.readers {
		// More readers than the buffer was sized for: they share no release slot and
		// would never reclaim. Caller error; refuse rather than corrupt the low-water.
		panic("buffer: more Reader() calls than declared readers")
	}
	b.assigned++
	return &cursor{b: b, id: id, pos: -1}
}

// put stores r at absolute index i, growing chunks as needed. Caller holds mu.
func (b *buffer) put(i int, r facade.Record) {
	c := i / b.chunkSize
	for len(b.chunks) <= c {
		b.chunks = append(b.chunks, make([]facade.Record, b.chunkSize))
	}
	b.chunks[c][i%b.chunkSize] = r
}

// at fetches the record at absolute index i. Caller holds mu; i must be < length and
// >= released (not yet reclaimed).
func (b *buffer) at(i int) facade.Record {
	return b.chunks[i/b.chunkSize][i%b.chunkSize]
}

// reclaim frees every record below the slowest cursor. Caller holds mu.
func (b *buffer) reclaim() {
	low := b.length
	for _, c := range b.cursors {
		if c < low {
			low = c
		}
	}
	for b.released < low {
		b.chunks[b.released/b.chunkSize][b.released%b.chunkSize] = nil
		b.released++
	}
}

// cursor is one reader's pull position over the buffer.
type cursor struct {
	b      *buffer
	id     int
	pos    int // absolute index of the current record; -1 before the first Next
	cur    facade.Record
	closed bool // guards Close against double-counting
}

func (c *cursor) Next(ctx context.Context) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	stop := context.AfterFunc(ctx, func() {
		c.b.mu.Lock()
		c.b.wake.Broadcast()
		c.b.mu.Unlock()
	})
	defer stop()

	c.b.mu.Lock()
	defer c.b.mu.Unlock()

	next := c.pos + 1
	for {
		if next < c.b.length {
			c.pos = next
			c.cur = c.b.at(next)
			c.b.cursors[c.id] = next + 1 // consumed through next
			c.b.reclaim()
			c.b.wake.Broadcast() // let a back-pressured Append proceed
			return true
		}
		if c.b.done { // caught up and producer finished → EOF
			return false
		}
		if err := ctx.Err(); err != nil {
			return false
		}
		c.b.wake.Wait()
	}
}

func (c *cursor) Record() facade.Record {
	return c.cur
}

func (c *cursor) Err() error {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	return c.b.err
}

// Close retires this reader (sentinel = never holds back the low-water mark), letting
// the rest of the buffer be reclaimed even if more records are appended afterwards.
func (c *cursor) Close() error {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.b.closed++
	c.b.cursors[c.id] = math.MaxInt
	c.b.reclaim()
	c.b.wake.Broadcast() // wake a blocked Append so it can observe all-readers-closed
	return nil
}
