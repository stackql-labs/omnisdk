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
