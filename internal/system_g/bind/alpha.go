package bind

import (
	"fmt"
	"io"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

var (
	_ facade.Alpha = delayEdge{}
	_ facade.Alpha = gateEdge{}
)

// delayEdge is a behavioural (E_α) edge carrying a single annotation: a traversal delay. The
// bind join honours it before invoking the inner (ctx-aware), so it is a real wait, not a busy
// spin. More annotations (Σ events, Signal) can join later without changing the formalism.
type delayEdge struct {
	d time.Duration
}

// NewDelayEdge builds a behavioural edge that delays traversal by d.
func NewDelayEdge(d time.Duration) facade.Alpha {
	return delayEdge{d: d}
}

func (e delayEdge) Delay() time.Duration { return e.d }
func (e delayEdge) On() string           { return "" }

func (e delayEdge) WriteTo(w io.Writer) (int64, error) {
	n, err := fmt.Fprintf(w, "alpha(delay=%s)", e.d)
	return int64(n), err
}

// gateEdge is a behavioural edge that fires when its source branch chooses arm: control, not data.
type gateEdge struct{ arm string }

// NewGateEdge builds the edge from a branch's arm to a node that runs only when that arm is chosen.
func NewGateEdge(arm string) facade.Alpha { return gateEdge{arm: arm} }

func (e gateEdge) Delay() time.Duration { return 0 }
func (e gateEdge) On() string           { return e.arm }

func (e gateEdge) WriteTo(w io.Writer) (int64, error) {
	n, err := fmt.Fprintf(w, "alpha(on=%s)", e.arm)
	return int64(n), err
}
