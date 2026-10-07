package plan

import (
	"slices"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// ControlKey is the row attribute a branch records its chosen arm under. It is control, not data: it
// never names a value a request sends, and leaves no result.
func ControlKey(branch string) string { return "\x00arm:" + branch }

// skippedKey marks the output of a node whose gates did not fire for a row.
const skippedKey = "\x00skipped"

// NewBranchSpec is a branch: an exchange that makes no request. Once per row it chooses one arm from
// the inputs it reads — the first whose condition holds, an unconditional last arm catching the rest
// — and records the choice under ControlKey(name). choose returns "" where no arm holds, and then no
// gate fires.
func NewBranchSpec(name string, in []string, choose func(bound map[string]any) (string, error)) ExchangeSpec {
	return NewExchangeSpec(name, in, []string{ControlKey(name)}, func(bound map[string]any) facade.Operator {
		arm, err := choose(bound)
		if err != nil {
			return errorOp{err: err}
		}
		return staticRecords{bind.NewDocRecord(map[string]any{ControlKey(name): arm})}
	}, nil)
}

// gate is one arm of one branch a node runs on.
type gate struct{ control, arm string }

// controlled is p with its gates made executable: each gated node wrapped to run only on a row where
// one of its gates fired, and each gate's choice carried to its node by an edge from the branch, so
// validation, ordering and binding treat it as they treat every other input.
func controlled(p Plan) Plan {
	gates := map[string][]gate{}
	var betas []BetaEdge
	for _, a := range p.Alphas() {
		if a.Alpha() == nil || a.Alpha().On() == "" {
			continue
		}
		g := gate{control: ControlKey(a.From()), arm: a.Alpha().On()}
		if !slices.Contains(gates[a.To()], g) {
			gates[a.To()] = append(gates[a.To()], g)
		}
		betas = append(betas, NewBetaEdge(a.From(), a.To(), g.control, g.control))
	}
	if len(gates) == 0 {
		return p
	}
	exchanges := make([]ExchangeSpec, 0, len(p.Exchanges()))
	for _, x := range p.Exchanges() {
		if gs, ok := gates[x.Name()]; ok {
			x = gatedSpec{ExchangeSpec: x, gates: gs}
		}
		exchanges = append(exchanges, x)
	}
	return controlledPlan{Plan: p, exchanges: exchanges, betas: append(slices.Clone(p.Betas()), betas...)}
}

type controlledPlan struct {
	Plan
	exchanges []ExchangeSpec
	betas     []BetaEdge
}

func (c controlledPlan) Exchanges() []ExchangeSpec { return c.exchanges }
func (c controlledPlan) Betas() []BetaEdge         { return c.betas }

// gatedSpec runs its exchange only on a row where one of its gates fired: a node several arms lead
// to runs when any of them is chosen. On any other row it makes no request, and the row passes
// through untouched — a dead arm never drops a row.
type gatedSpec struct {
	ExchangeSpec
	gates []gate
}

func (g gatedSpec) In() []string {
	in := slices.Clone(g.ExchangeSpec.In())
	for _, x := range g.gates {
		if !slices.Contains(in, x.control) {
			in = append(in, x.control)
		}
	}
	return in
}

func (g gatedSpec) Make(bound map[string]any) facade.Operator {
	fired := false
	for _, x := range g.gates {
		if arm, _ := bound[x.control].(string); arm != "" && arm == x.arm {
			fired = true
			break
		}
	}
	if !fired {
		return staticRecords{bind.NewDocRecord(map[string]any{skippedKey: true})}
	}
	inputs := make(map[string]any, len(bound))
	for k, v := range bound {
		if !slices.ContainsFunc(g.gates, func(x gate) bool { return x.control == k }) {
			inputs[k] = v
		}
	}
	return g.ExchangeSpec.Make(inputs)
}

func (g gatedSpec) Flatten() facade.Transform { return gatedFlatten{inner: flattenOf(g.ExchangeSpec)} }

// gatedFlatten passes a skipped node's row through as it arrived, and merges anything else as the
// node's own flatten does.
type gatedFlatten struct{ inner facade.Transform }

func (f gatedFlatten) Apply(in facade.Page) (facade.Record, error) {
	m, ok := bind.DocMap(in)
	if !ok {
		return f.inner.Apply(in)
	}
	if out, _ := m[bind.KeyOutput].(map[string]any); out != nil && out[skippedKey] == true {
		input, _ := m[bind.KeyInput].(map[string]any)
		return bind.NewDocRecord(input), nil
	}
	return f.inner.Apply(in)
}
