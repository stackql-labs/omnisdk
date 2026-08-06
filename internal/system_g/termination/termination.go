package termination

import (
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
