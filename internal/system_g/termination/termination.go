package termination

import (
	"errors"
	"fmt"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

var (
	_ facade.TerminationPolicy = maxIterations{}
	_ facade.TerminationPolicy = budget{}
	_ facade.TerminationPolicy = deadline{}
	_ facade.TerminationPolicy = any{}
	_ facade.TerminationPolicy = all{}
)

// maxIterations stops after n rounds (the iteration-bound / cycle-bound form).
type maxIterations struct{ n int }

// NewMaxIterations bounds a loop to n rounds.
func NewMaxIterations(n int) facade.TerminationPolicy { return maxIterations{n: n} }

func (m maxIterations) Stop(p facade.Progress) bool { return p.Round >= m.n }

// budget stops after n records have been emitted.
type budget struct{ n int }

// NewBudget bounds a loop to n emitted records.
func NewBudget(n int) facade.TerminationPolicy { return budget{n: n} }

func (b budget) Stop(p facade.Progress) bool { return p.Emitted >= b.n }

// deadline stops once wall-clock passes a fixed instant (TTL form).
type deadline struct{ at time.Time }

// NewDeadline bounds a loop to d from now.
func NewDeadline(d time.Duration) facade.TerminationPolicy {
	return deadline{at: time.Now().Add(d)}
}

func (d deadline) Stop(_ facade.Progress) bool { return time.Now().After(d.at) }

// any stops when ANY child policy stops (logical OR — the usual "first bound wins").
type any struct{ policies []facade.TerminationPolicy }

// NewAny composes policies disjunctively.
func NewAny(ps ...facade.TerminationPolicy) facade.TerminationPolicy { return any{policies: ps} }

func (a any) Stop(p facade.Progress) bool {
	for _, x := range a.policies {
		if x.Stop(p) {
			return true
		}
	}
	return false
}

// all stops only when EVERY child policy stops (logical AND).
type all struct{ policies []facade.TerminationPolicy }

// NewAll composes policies conjunctively.
func NewAll(ps ...facade.TerminationPolicy) facade.TerminationPolicy { return all{policies: ps} }

func (a all) Stop(p facade.Progress) bool {
	for _, x := range a.policies {
		if !x.Stop(p) {
			return false
		}
	}
	return len(a.policies) > 0
}

// Specs: termination as data, decided well-founded before anything runs. Each measure moves one way
// toward its bound every round a loop continues — rounds count up, records are only ever added (a
// round adding none is a fixpoint and stops), time only advances — so a positive bound on any of them
// is reached. A bound of zero or less is no bound.

var (
	_ facade.TerminationSpec = rounds(0)
	_ facade.TerminationSpec = records(0)
	_ facade.TerminationSpec = within(0)
	_ facade.TerminationSpec = anyOf{}
	_ facade.TerminationSpec = allOf{}
)

type rounds int

// Rounds stops a loop after n rounds.
func Rounds(n int) facade.TerminationSpec { return rounds(n) }

func (r rounds) WellFounded() error {
	if r < 1 {
		return fmt.Errorf("termination: rounds %d bounds nothing; it must be at least 1", int(r))
	}
	return nil
}

func (r rounds) Policy(time.Time) facade.TerminationPolicy { return NewMaxIterations(int(r)) }

type records int

// Records stops a loop once it has emitted n records.
func Records(n int) facade.TerminationSpec { return records(n) }

func (r records) WellFounded() error {
	if r < 1 {
		return fmt.Errorf("termination: records %d bounds nothing; it must be at least 1", int(r))
	}
	return nil
}

func (r records) Policy(time.Time) facade.TerminationPolicy { return NewBudget(int(r)) }

type within time.Duration

// Within stops a loop once d has passed since it started.
func Within(d time.Duration) facade.TerminationSpec { return within(d) }

func (w within) WellFounded() error {
	if w <= 0 {
		return fmt.Errorf("termination: within %s bounds nothing; it must be positive", time.Duration(w))
	}
	return nil
}

func (w within) Policy(start time.Time) facade.TerminationPolicy {
	return deadline{at: start.Add(time.Duration(w))}
}

type anyOf []facade.TerminationSpec

// AnyOf stops a loop when any of specs would: the first bound reached wins. It is well-founded when
// at least one of them is.
func AnyOf(specs ...facade.TerminationSpec) facade.TerminationSpec { return anyOf(specs) }

func (a anyOf) WellFounded() error {
	var why []error
	for _, s := range a {
		err := s.WellFounded()
		if err == nil {
			return nil
		}
		why = append(why, err)
	}
	if len(why) == 0 {
		return fmt.Errorf("termination: any of nothing bounds nothing")
	}
	return fmt.Errorf("termination: none of the alternatives bounds the loop: %w", errors.Join(why...))
}

func (a anyOf) Policy(start time.Time) facade.TerminationPolicy {
	ps := make([]facade.TerminationPolicy, 0, len(a))
	for _, s := range a {
		// An alternative that bounds nothing never fires; leaving it out changes nothing but keeps a
		// zero bound from stopping a loop before its first round.
		if s.WellFounded() == nil {
			ps = append(ps, s.Policy(start))
		}
	}
	return NewAny(ps...)
}

type allOf []facade.TerminationSpec

// AllOf stops a loop only once every one of specs would. It is well-founded only when each of them
// is: one that never fires holds the loop open forever.
func AllOf(specs ...facade.TerminationSpec) facade.TerminationSpec { return allOf(specs) }

func (a allOf) WellFounded() error {
	if len(a) == 0 {
		return fmt.Errorf("termination: all of nothing bounds nothing")
	}
	for _, s := range a {
		if err := s.WellFounded(); err != nil {
			return fmt.Errorf("termination: every condition must bound the loop: %w", err)
		}
	}
	return nil
}

func (a allOf) Policy(start time.Time) facade.TerminationPolicy {
	ps := make([]facade.TerminationPolicy, 0, len(a))
	for _, s := range a {
		ps = append(ps, s.Policy(start))
	}
	return NewAll(ps...)
}
