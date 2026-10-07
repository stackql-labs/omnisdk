package termination_test

import (
	"testing"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/termination"
)

func TestMaxIterations(t *testing.T) {
	p := termination.NewMaxIterations(3)
	cases := []struct {
		round int
		stop  bool
	}{
		{0, false},
		{2, false},
		{3, true},
		{4, true},
	}
	for _, c := range cases {
		if got := p.Stop(facade.Progress{Round: c.round}); got != c.stop {
			t.Errorf("Stop(Round=%d) = %v, want %v", c.round, got, c.stop)
		}
	}
}

func TestBudget(t *testing.T) {
	p := termination.NewBudget(2)
	cases := []struct {
		emitted int
		stop    bool
	}{
		{0, false},
		{1, false},
		{2, true},
		{5, true},
	}
	for _, c := range cases {
		if got := p.Stop(facade.Progress{Emitted: c.emitted}); got != c.stop {
			t.Errorf("Stop(Emitted=%d) = %v, want %v", c.emitted, got, c.stop)
		}
	}
}

func TestDeadline(t *testing.T) {
	if got := termination.NewDeadline(-time.Second).Stop(facade.Progress{}); !got {
		t.Error("elapsed deadline = false, want true")
	}
	if got := termination.NewDeadline(time.Hour).Stop(facade.Progress{}); got {
		t.Error("future deadline = true, want false")
	}
}

func TestAny(t *testing.T) {
	p := termination.NewAny(
		termination.NewMaxIterations(5),
		termination.NewBudget(2),
	)
	cases := []struct {
		prog facade.Progress
		stop bool
	}{
		{facade.Progress{Round: 1, Emitted: 1}, false},
		{facade.Progress{Round: 5, Emitted: 0}, true},
		{facade.Progress{Round: 0, Emitted: 2}, true},
	}
	for _, c := range cases {
		if got := p.Stop(c.prog); got != c.stop {
			t.Errorf("Stop(%+v) = %v, want %v", c.prog, got, c.stop)
		}
	}
}

func TestAnyEmpty(t *testing.T) {
	if termination.NewAny().Stop(facade.Progress{Round: 1 << 30}) {
		t.Error("empty Any stopped, want never")
	}
}

func TestAll(t *testing.T) {
	p := termination.NewAll(
		termination.NewMaxIterations(5),
		termination.NewBudget(2),
	)
	cases := []struct {
		prog facade.Progress
		stop bool
	}{
		{facade.Progress{Round: 5, Emitted: 1}, false},
		{facade.Progress{Round: 4, Emitted: 2}, false},
		{facade.Progress{Round: 5, Emitted: 2}, true},
	}
	for _, c := range cases {
		if got := p.Stop(c.prog); got != c.stop {
			t.Errorf("Stop(%+v) = %v, want %v", c.prog, got, c.stop)
		}
	}
}

func TestAllEmpty(t *testing.T) {
	// vacuous AND must not stop
	if termination.NewAll().Stop(facade.Progress{}) {
		t.Error("empty All stopped, want never")
	}
}

func TestNestedComposition(t *testing.T) {
	p := termination.NewAny(
		termination.NewAll(
			termination.NewMaxIterations(2),
			termination.NewBudget(1),
		),
		termination.NewDeadline(-time.Second),
	)
	if !p.Stop(facade.Progress{}) {
		t.Error("nested Any with elapsed deadline = false, want true")
	}
}

// A spec is well-founded exactly when its stop rests on a positive bound; "any" needs one such
// alternative, "all" needs every condition to be one.
func TestSpecWellFounded(t *testing.T) {
	for _, c := range []struct {
		name string
		spec facade.TerminationSpec
		ok   bool
	}{
		{"rounds", termination.Rounds(3), true},
		{"zero rounds", termination.Rounds(0), false},
		{"records", termination.Records(10), true},
		{"negative records", termination.Records(-1), false},
		{"within", termination.Within(time.Second), true},
		{"zero within", termination.Within(0), false},
		{"any with one bound", termination.AnyOf(termination.Rounds(0), termination.Within(time.Second)), true},
		{"any with none", termination.AnyOf(termination.Rounds(0), termination.Records(0)), false},
		{"empty any", termination.AnyOf(), false},
		{"all bounded", termination.AllOf(termination.Rounds(2), termination.Records(5)), true},
		{"all with one unbounded", termination.AllOf(termination.Rounds(2), termination.Records(0)), false},
		{"empty all", termination.AllOf(), false},
	} {
		if err := c.spec.WellFounded(); (err == nil) != c.ok {
			t.Errorf("%s: WellFounded = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

// A spec's policy stops where it says: rounds by round, records by emitted, within by the clock from
// the loop's own start.
func TestSpecPolicy(t *testing.T) {
	now := time.Now()
	if p := termination.Rounds(2).Policy(now); p.Stop(facade.Progress{Round: 1}) || !p.Stop(facade.Progress{Round: 2}) {
		t.Error("rounds(2)")
	}
	if p := termination.Records(3).Policy(now); p.Stop(facade.Progress{Emitted: 2}) || !p.Stop(facade.Progress{Emitted: 3}) {
		t.Error("records(3)")
	}
	if p := termination.Within(time.Hour).Policy(now.Add(-2 * time.Hour)); !p.Stop(facade.Progress{}) {
		t.Error("within counts from the loop's start")
	}
	if p := termination.AnyOf(termination.Rounds(0), termination.Rounds(5)).Policy(now); p.Stop(facade.Progress{Round: 0}) {
		t.Error("an unbounded alternative must not stop the loop before it starts")
	}
}
