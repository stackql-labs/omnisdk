package scc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

var (
	_ facade.SCC = &scc{}
)

// Reason records why the fixpoint loop stopped (observable for tests/tracing).
type Reason int32

const (
	Running    Reason = iota
	Fixpoint          // a round produced no new records
	Terminated        // a TerminationPolicy fired (well-founded backstop, §W)
)

// Step runs one round of the condensed cycle: given the current frontier of records,
// produce the records derived this round. An empty result signals a fixpoint. This is
// where the SCC's member exchanges are exercised; the scc operator owns the iteration.
type Step interface {
	Round(ctx context.Context, frontier []facade.Record) ([]facade.Record, error)
}

// Keyer yields a dedup key; two records with the same key are "the same" for fixpoint
// detection (semi-naive evaluation).
type Keyer func(facade.Record) string

// scc is a condensed strongly-connected-component operator (Tarjan condensation, §W).
// From outside it is an ordinary pull operator; inside it converts the data-flow cycle
// into a bounded fixpoint loop, so cascading pull never recurses infinitely. It emits
// the transitive closure (seed + all derived records) outward, streaming.
type scc struct {
	id       int64
	emits    []facade.Beta
	receives []facade.Beta

	seed    facade.Operator          // supplies the initial frontier (inbound β into the SCC)
	step    Step                     // one round of the cycle
	key     Keyer                    // dedup / fixpoint key
	policy  facade.TerminationPolicy // well-founded backstop; nil = rely on fixpoint only
	readers int                      // out-degree (buffer reader count)

	// observable termination state (for tests/tracing); safe to read after EOF.
	iterations atomic.Int64
	reason     atomic.Int32
}

// NewSCC builds a condensed-cycle operator. `seed` feeds the initial frontier, `step`
// runs one round of the cycle, `key` detects already-seen records, `policy` is the
// well-founded backstop (nil = rely on fixpoint only), `readers` is the out-degree.
func NewSCC(
	id int64,
	seed facade.Operator,
	step Step,
	key Keyer,
	policy facade.TerminationPolicy,
	readers int,
) facade.SCC {
	return &scc{
		id:       id,
		seed:     seed,
		step:     step,
		key:      key,
		policy:   policy,
		readers:  readers,
		emits:    []facade.Beta{},
		receives: []facade.Beta{},
	}
}

func (s *scc) Emits() []facade.Beta    { return s.emits }
func (s *scc) Receives() []facade.Beta { return s.receives }

func (s *scc) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write([]byte(fmt.Sprintf("SCC id = %d\n", s.id)))
	return int64(n), err
}

// Iterations and TerminationReason expose the loop outcome once the stream has drained.
func (s *scc) Iterations() int           { return int(s.iterations.Load()) }
func (s *scc) TerminationReason() Reason { return Reason(s.reason.Load()) }

func (s *scc) Open(ctx context.Context) facade.Records {
	readers := s.readers
	if readers < 1 {
		readers = 1
	}
	buf := buffer.NewBuffer(readers, 1024, 0)

	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()

		seen := map[string]struct{}{}
		emitted := 0

		// emit publishes a record; returns false if the producer should stop (readers
		// gone → clean stop, or real error → recorded in cerr).
		emit := func(r facade.Record) bool {
			if err := buf.Append(ctx, r); err != nil {
				if !errors.Is(err, buffer.ErrAllReadersClosed) {
					cerr = err
				}
				return false
			}
			emitted++
			return true
		}

		// Seed the frontier from the inbound operator, emitting each reached record.
		frontier := []facade.Record{}
		in := s.seed.Open(ctx)
		for in.Next(ctx) {
			r := in.Record()
			k := s.key(r)
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			frontier = append(frontier, r)
			if !emit(r) {
				in.Close()
				return
			}
		}
		if err := in.Err(); err != nil {
			cerr = err
			in.Close()
			return
		}
		in.Close()

		// Semi-naive fixpoint: iterate the cycle over the frontier until a round adds
		// nothing new (fixpoint) or the iteration cap fires (bound).
		for len(frontier) > 0 {
			if s.policy != nil && s.policy.Stop(facade.Progress{
				Round:   int(s.iterations.Load()),
				Emitted: emitted,
			}) {
				s.reason.Store(int32(Terminated))
				return
			}
			s.iterations.Add(1)

			derived, err := s.step.Round(ctx, frontier)
			if err != nil {
				cerr = err
				return
			}

			var next []facade.Record
			for _, r := range derived {
				k := s.key(r)
				if _, ok := seen[k]; ok {
					continue
				}
				seen[k] = struct{}{}
				next = append(next, r)
				if !emit(r) {
					return
				}
			}
			if len(next) == 0 {
				s.reason.Store(int32(Fixpoint))
				return
			}
			frontier = next
		}
		s.reason.Store(int32(Fixpoint))
	}()

	return buf.Reader()
}
