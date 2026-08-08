package plan

import (
	"context"
	"fmt"
	"io"
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
// accumulates, any node can bind from any upstream, not just its immediate predecessor. This
// handles any ACYCLIC DAG (diamonds, fan-in/out included). Cyclic plans (SCCs) are rejected up
// front — fixpoint execution over a Tarjan condensation is future work, but a cycle must never be
// silently dropped.

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
	if err := Validate(p); err != nil {
		return nil, err
	}
	byName := make(map[string]ExchangeSpec, len(p.Exchanges()))
	for _, x := range p.Exchanges() {
		byName[x.Name()] = x
	}
	order := topoOrder(p.Exchanges(), p.Betas())
	if len(order) != len(byName) {
		// Kahn's dropped nodes → a β dependency cycle. Reject, never silently omit them.
		return nil, fmt.Errorf("plan: dependency cycle among exchanges (SCC execution not yet supported)")
	}

	nid := id
	// With κ inputs, the seed row is the inputs and every exchange is a stage; otherwise the
	// first exchange is the root (its output need not be an agnostic row — e.g. raw pages).
	var op facade.Operator
	stages := order
	if len(p.Inputs()) > 0 {
		op = tap(&nid, seedOp{rec: bind.NewDocRecord(p.Inputs())}, "inputs")
	} else if byName[order[0]].Flatten() != nil {
		// The root DECLARES how its output merges into the running row — so it needs a row to merge
		// INTO. Seed an empty one and run it as an ordinary stage. Made a bare root instead, its
		// Flatten was silently dropped: whatever it declares in Out was never extracted, and every β
		// edge reading that attribute bound nothing. That is how a client_credentials Auth root
		// obtained a token and still issued unauthenticated requests — the provider's 401 was the
		// first sign, three hops from the cause.
		op = tap(&nid, seedOp{rec: bind.NewDocRecord(map[string]any{})}, "inputs")
	} else {
		op = tap(&nid, byName[order[0]].Make(nil), order[0])
		stages = order[1:]
	}

	for _, name := range stages {
		node := byName[name]
		var bindings []bind.Binding
		for _, attr := range node.In() {
			if src, ok := betaSrc(p.Betas(), name, attr); ok {
				bindings = append(bindings, bind.NewBinding(src, attr))
			} else {
				bindings = append(bindings, bind.NewBinding(attr, attr)) // κ/env by name
			}
		}
		nid++
		op = bind.NewBindJoin(nid, op, bindings, bind.InnerFactory(node.Make), alphaInto(p.Alphas(), name), 1)

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

// topoOrder returns exchange names root-first by β dependency (Kahn's, declaration order).
// Edges from a non-exchange (κ inputs, From "") are bindings, not ordering deps.
func topoOrder(exchanges []ExchangeSpec, betas []BetaEdge) []string {
	indeg := make(map[string]int)
	names := make([]string, 0, len(exchanges))
	for _, x := range exchanges {
		if _, ok := indeg[x.Name()]; !ok {
			indeg[x.Name()] = 0
			names = append(names, x.Name())
		}
	}
	adj := make(map[string][]string)
	edgeSeen := make(map[string]bool)
	for _, e := range betas {
		if _, ok := indeg[e.From()]; !ok {
			continue
		}
		k := e.From() + "\x00" + e.To()
		if edgeSeen[k] {
			continue
		}
		edgeSeen[k] = true
		adj[e.From()] = append(adj[e.From()], e.To())
		indeg[e.To()]++
	}

	var queue []string
	for _, n := range names {
		if indeg[n] == 0 {
			queue = append(queue, n)
		}
	}
	order := make([]string, 0, len(names))
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		order = append(order, n)
		for _, m := range adj[n] {
			indeg[m]--
			if indeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}
	return order
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

func alphaInto(alphas []AlphaEdge, to string) facade.Alpha {
	for _, a := range alphas {
		if a.To() == to {
			return a.Alpha()
		}
	}
	return nil
}
