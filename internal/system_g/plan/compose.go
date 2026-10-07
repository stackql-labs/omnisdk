package plan

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	encoder "github.com/stackql-labs/omnisdk/internal/system_g/endec"
	"github.com/stackql-labs/omnisdk/internal/system_g/exchange"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// The build → select → compose layer. Exchanges and β edges are declared (static; from docs
// eventually); a Plan is the selected slice for a goal; Compose builds the executable operator,
// late-binding the runtime α edges.
//
// Compose realises the plan as a staged pipeline in topological order: the root operator, then
// one stage per downstream exchange. Each stage is a bind join (binding every β slot into that
// node — so an attribute like an auth token declared from the root fans to all nodes) followed
// by a flatten that merges the produced output back into the running row. Because the row
// accumulates, any node can bind from any upstream, not just its immediate predecessor. Stages
// follow the Tarjan condensation: an acyclic node is one stage, and a strongly connected component
// is one stage that runs the cycle to a fixpoint under a termination policy proved well-founded
// before the plan runs (see cycle.go).

// Compose realises a plan as a staged pipeline in topological order: root, then a
// bind-join + flatten + output-tap stage per downstream exchange, then the sink. The row
// accumulates, so any node binds from any upstream (a κ input or auth token fans to all). Every
// exchange taps the output (the bowtie). Rejects an invalid plan instantly, before any run.
func Compose(id int64, p Plan, w io.Writer) facade.Operator {
	op, err := composePipeline(id, p)
	if err != nil {
		return errorOp{err: err}
	}
	enc := p.Encoder()
	if enc == nil {
		enc = encoder.NewJSONLEncoder()
	}
	return NewOutputExchange(id, op, p.Egress(), enc, w)
}

// ComposeRows realises a plan as a ROW CURSOR: the same staged pipeline as Compose, but the terminal
// applies egress and EMITS the shaped records for a library consumer to iterate (Open → Records),
// rather than encoding them to a writer. This is the seam the pkg/omnisdk facade builds on.
func ComposeRows(id int64, p Plan) facade.Operator {
	op, err := composePipeline(id, p)
	if err != nil {
		return errorOp{err: err}
	}
	return newRowsOutput(op, p.Egress())
}

// composePipeline builds the staged bind-join pipeline (root → stages, egress-unshaped), shared by
// Compose (byte terminal) and ComposeRows (row terminal). It stops before the terminal.
func composePipeline(id int64, p Plan) (facade.Operator, error) {
	p = controlled(p)
	if err := Validate(p); err != nil {
		return nil, err
	}
	byName := make(map[string]ExchangeSpec, len(p.Exchanges()))
	for _, x := range p.Exchanges() {
		byName[x.Name()] = x
	}
	comps := condense(p.Exchanges(), p.Betas(), p.Alphas())
	// Every cycle is proved to stop before anything runs: a cycle whose termination is not
	// well-founded, or that nothing outside it can start, is refused here rather than discovered
	// mid-run.
	specs := make([]facade.TerminationSpec, len(comps))
	for i, c := range comps {
		if !c.cyclic {
			continue
		}
		spec, err := cycleSpec(p, c)
		if err != nil {
			return nil, err
		}
		specs[i] = spec
	}

	nid := id
	// With κ inputs, the seed row is the inputs and every exchange is a stage; otherwise the
	// first exchange is the root (its output need not be an agnostic row — e.g. raw pages).
	var op facade.Operator
	stages := comps
	if len(p.Inputs()) > 0 {
		op = tap(&nid, seedOp{rec: bind.NewDocRecord(p.Inputs())}, "inputs")
	} else if comps[0].cyclic || byName[comps[0].members[0]].Flatten() != nil {
		// The root DECLARES how its output merges into the running row — so it needs a row to merge
		// INTO. Seed an empty one and run it as an ordinary stage. Made a bare root instead, its
		// Flatten was silently dropped: whatever it declares in Out was never extracted, and every β
		// edge reading that attribute bound nothing. That is how a client_credentials Auth root
		// obtained a token and still issued unauthenticated requests — the provider's 401 was the
		// first sign, three hops from the cause. A cycle at the root needs a row for the same reason.
		op = tap(&nid, seedOp{rec: bind.NewDocRecord(map[string]any{})}, "inputs")
	} else {
		name := comps[0].members[0]
		op = tap(&nid, byName[name].Make(nil), name)
		stages = comps[1:]
	}

	for _, c := range stages {
		if c.cyclic {
			spec := specs[slices.IndexFunc(comps, func(x component) bool { return x.members[0] == c.members[0] })]
			stage, err := newCycleStage(p, byName, c, op, spec)
			if err != nil {
				return nil, err
			}
			op = tap(&nid, stage, strings.Join(c.members, "+"))
			continue
		}
		name := c.members[0]
		node := byName[name]
		var bindings []bind.Binding
		for _, attr := range node.In() {
			// An input may have several sources; any one satisfies it, the first with a value
			// winning (see the join).
			found := false
			for _, e := range p.Betas() {
				if e.To() == name && e.Tgt() == attr {
					bindings = append(bindings, bind.NewBinding(e.Src(), attr))
					found = true
				}
			}
			if !found {
				bindings = append(bindings, bind.NewBinding(attr, attr)) // κ/env by name
			}
		}
		nid++
		op = bind.NewBindJoinIn(nid, op, bindings, bind.InnerFactory(node.Make), alphaInto(p.Alphas(), name), 1, node.Inbound())

		flat := node.Flatten()
		if flat == nil {
			flat = bind.NewTupleFlatten()
		}
		nid++
		op = exchange.NewTransformExchange(nid, op, flat, 1)
		op = tap(&nid, op, name) // bowtie: every exchange → output
	}
	return op, nil
}

// Order is the order Compose runs a plan's exchanges in: components of the condensation in
// dependency order, a cycle's members together in declaration order.
func Order(exchanges []ExchangeSpec, betas []BetaEdge) []string {
	var out []string
	for _, c := range condense(exchanges, betas, nil) {
		out = append(out, c.members...)
	}
	return out
}

// Validate is the AOT correctness check: every declared input (In) of every exchange must be
// satisfiable — by a β edge that feeds it, or by a κ input present (and non-empty) in Inputs.
// An input with neither is unsatisfiable and the plan is rejected before execution.
func Validate(p Plan) error {
	var missing []string
	for _, x := range p.Exchanges() {
		for _, attr := range x.In() {
			if _, ok := betaSrc(p.Betas(), x.Name(), attr); ok {
				continue // produced by an upstream exchange at runtime
			}
			if v, ok := p.Inputs()[attr]; ok && !emptyValue(v) {
				continue // supplied as a κ input
			}
			missing = append(missing, x.Name()+"."+attr)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("plan: unsatisfied required inputs (no β source or κ input): %s", strings.Join(missing, ", "))
	}
	return nil
}

func emptyValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && s == ""
}

// errorOp is an operator that rejects immediately on Open — no exchange runs. Used to surface a
// plan validation failure through the normal pull path (rs.Err()).
type errorOp struct {
	err error
}

func (e errorOp) Open(context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1, 0)
	buf.Complete(e.err)
	return buf.Reader()
}

// seedOp emits a single row — the κ inputs — to seed a pipeline whose nodes bind from it.
type seedOp struct {
	rec facade.Record
}

func (s seedOp) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1, 0)
	go func() {
		defer buf.Complete(nil)
		_ = buf.Append(ctx, s.rec)
	}()
	return buf.Reader()
}

// tap inserts a log tap (an exchange's β edge into the output) that logs the record under label
// and passes it through. Advances nid.
func tap(nid *int64, op facade.Operator, label string) facade.Operator {
	*nid++
	return NewLogTap(*nid, op, label, 1)
}

// betaSrc returns the source attribute of the β edge feeding attr into the exchange to, if any.
func betaSrc(betas []BetaEdge, to, attr string) (string, bool) {
	for _, e := range betas {
		if e.To() == to && e.Tgt() == attr {
			return e.Src(), true
		}
	}
	return "", false
}

// alphaInto is the timing annotation on the edges into to: the first α with a delay. Gates carry no
// timing; they are applied by controlled.
func alphaInto(alphas []AlphaEdge, to string) facade.Alpha {
	for _, a := range alphas {
		if a.To() == to && a.Alpha() != nil && a.Alpha().Delay() > 0 {
			return a.Alpha()
		}
	}
	return nil
}
