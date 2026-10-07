package omnisdk

import (
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/termination"
)

// Termination bounds a cycle: a node wired to itself, or several wired to each other. Every cycle
// needs one, and it must be well-founded — resting on rounds, records or time, never on "stop when
// nothing new arrives" alone — which is decided when the query is planned, before anything runs.
type Termination interface {
	// WellFounded is nil when the termination provably stops a loop, and otherwise says why not.
	WellFounded() error
	spec() facade.TerminationSpec
}

type terminationSpec struct{ s facade.TerminationSpec }

func (t terminationSpec) WellFounded() error           { return t.s.WellFounded() }
func (t terminationSpec) spec() facade.TerminationSpec { return t.s }

// Rounds stops a cycle after n rounds.
func Rounds(n int) Termination { return terminationSpec{termination.Rounds(n)} }

// Records stops a cycle once it has produced n rows.
func Records(n int) Termination { return terminationSpec{termination.Records(n)} }

// Within stops a cycle once d has passed since it started.
func Within(d time.Duration) Termination { return terminationSpec{termination.Within(d)} }

// AnyOf stops a cycle at the first of ts reached; it is well-founded when one of them is.
func AnyOf(ts ...Termination) Termination {
	return terminationSpec{termination.AnyOf(specsOf(ts)...)}
}

// AllOf stops a cycle only once every one of ts is reached; it is well-founded only when each is.
func AllOf(ts ...Termination) Termination {
	return terminationSpec{termination.AllOf(specsOf(ts)...)}
}

func specsOf(ts []Termination) []facade.TerminationSpec {
	out := make([]facade.TerminationSpec, 0, len(ts))
	for _, t := range ts {
		if t != nil {
			out = append(out, t.spec())
		}
	}
	return out
}
