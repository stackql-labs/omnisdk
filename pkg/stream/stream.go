// Package stream writes a relation's rows straight to an output — a file, a pipe, a socket, a
// producer's buffer — framed as jsonl, delimited text, OTel logs or a text table.
//
// A sink picks the framing and nothing else: every cell is rendered by pkg/encode, so a value is
// the same bytes in every sink and in a host that renders it itself. Columns are written in the
// order declared; rows in the order written, never sorted.
//
// A sink never holds the relation. It holds at most one batch, written out when BatchSize rows are
// waiting, when FlushInterval has passed since the first of them arrived, and on Close — whichever
// comes first.
package stream

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/pkg/encode"
)

// Options is how a sink batches. The zero value writes every row as it arrives.
type Options struct {
	// BatchSize is how many rows wait before the batch is written. Zero or one writes each row.
	BatchSize int
	// FlushInterval bounds how long a row waits: a batch is written this long after its first row
	// arrived, full or not. Zero waits for BatchSize or Close alone.
	FlushInterval time.Duration
}

// Writer is a sink over one relation.
type Writer interface {
	// Write adds a row, keyed by column name. A column the row lacks is written as null; a key no
	// column declares is not written.
	Write(row map[string]any) error
	// Flush writes the waiting batch now.
	Flush() error
	// Close flushes and ends the relation. It does not close the underlying io.Writer.
	Close() error
	// Warnings is what the sink saw fit to report, each at most once per relation — a NaN or an
	// infinity written as null, for one.
	Warnings() []string
}

// Source is a cursor over rows; omnisdk.Rows is one.
type Source interface {
	Next() bool
	Row() map[string]any
	Err() error
}

// Copy writes every row of src to w and closes w. A failure of either side midway still writes
// what was already produced before the error is returned: no row taken from src is dropped silently.
func Copy(w Writer, src Source) error {
	for src.Next() {
		if err := w.Write(src.Row()); err != nil {
			return errors.Join(err, w.Close())
		}
	}
	return errors.Join(src.Err(), w.Close())
}

// framing is what differs between sinks: how a batch of rows becomes bytes.
type framing interface {
	// batch writes rows, each in column order, as one unit.
	batch(out io.Writer, rows [][]any) error
	// end writes whatever closes the relation, if anything.
	end(out io.Writer) error
}

// sink is the batching every framing shares.
type sink struct {
	mu      sync.Mutex
	out     io.Writer
	columns []string
	frame   framing
	opts    Options
	pending [][]any
	timer   *time.Timer
	closed  bool
	err     error // the first write failure; the sink refuses rows after it

	warned   map[string]bool
	warnings []string
}

func newSink(out io.Writer, columns []string, opts Options, f framing) (*sink, error) {
	if out == nil {
		return nil, fmt.Errorf("stream: no output")
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("stream: no columns")
	}
	if opts.BatchSize < 0 || opts.FlushInterval < 0 {
		return nil, fmt.Errorf("stream: negative batch size or flush interval")
	}
	return &sink{out: out, columns: append([]string(nil), columns...), frame: f, opts: opts,
		warned: map[string]bool{}}, nil
}

func (s *sink) Write(row map[string]any) error {
	cells := make([]any, len(s.columns))
	for i, c := range s.columns {
		cells[i] = row[c]
		if encode.NonFinite(cells[i]) {
			s.warn("non-finite", fmt.Sprintf("column %q held NaN or an infinity, written as null", c))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("stream: write after close")
	}
	if s.err != nil {
		return s.err
	}
	s.pending = append(s.pending, cells)
	if len(s.pending) >= max(s.opts.BatchSize, 1) {
		return s.flushLocked()
	}
	if s.opts.FlushInterval > 0 && s.timer == nil {
		s.timer = time.AfterFunc(s.opts.FlushInterval, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.timer = nil
			if !s.closed {
				_ = s.flushLocked() // kept in s.err, returned by the next Write, Flush or Close
			}
		})
	}
	return nil
}

func (s *sink) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *sink) flushLocked() error {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.err != nil {
		return s.err
	}
	if len(s.pending) == 0 {
		return nil
	}
	rows := s.pending
	s.pending = nil
	if err := s.frame.batch(s.out, rows); err != nil {
		s.err = fmt.Errorf("stream: %w", err)
	}
	return s.err
}

func (s *sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.err
	}
	err := s.flushLocked()
	s.closed = true
	if err != nil {
		return err
	}
	if err := s.frame.end(s.out); err != nil {
		s.err = fmt.Errorf("stream: %w", err)
	}
	return s.err
}

// warn records msg unless a warning of the same kind already was: once per relation, not per row.
func (s *sink) warn(kind, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.warned[kind] {
		s.warned[kind] = true
		s.warnings = append(s.warnings, msg)
	}
}

func (s *sink) Warnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.warnings...)
}
