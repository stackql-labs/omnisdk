package plan

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

type admissionPlan struct {
	Plan
	exchanges []ExchangeSpec
	betas     []BetaEdge
	alphas    []AlphaEdge
}

func (p admissionPlan) Exchanges() []ExchangeSpec { return p.exchanges }
func (p admissionPlan) Betas() []BetaEdge         { return p.betas }
func (p admissionPlan) Alphas() []AlphaEdge       { return p.alphas }

type externalBinding struct{ source, slot string }

// collapseAdmissions turns independent, exclusively consumed read observations
// and their admitting consumer into one progressive stage. All other staging is
// unchanged. Unsupported sharing/cycles are refused before effects, never run
// with misleading "early admission" semantics.
func collapseAdmissions(p Plan) (Plan, error) {
	components := condense(p.Exchanges(), p.Betas(), p.Alphas())
	for _, target := range p.Exchanges() {
		transform, ok := target.Inbound().(facade.InboxTransform)
		if !ok || transform.Admission() == nil {
			continue
		}
		for _, component := range components {
			if component.cyclic && slices.Contains(component.members, target.Name()) {
				return nil, fmt.Errorf("admission %s: cycles are not yet supported", target.Name())
			}
		}
		byName := map[string]ExchangeSpec{}
		for _, spec := range p.Exchanges() {
			byName[spec.Name()] = spec
		}
		members := map[string]bool{target.Name(): true}
		var sources []ExchangeSpec
		for _, edge := range p.Betas() {
			if edge.To() != target.Name() || edge.From() == "" || members[edge.From()] {
				continue
			}
			source := byName[edge.From()]
			marker, ok := source.(interface{ ReadOnly() bool })
			if ok && marker.ReadOnly() {
				sources = append(sources, source)
				members[source.Name()] = true
			}
		}
		if len(sources) == 0 {
			continue
		}
		for _, edge := range p.Betas() {
			if members[edge.From()] && edge.From() != target.Name() && edge.To() != target.Name() {
				return nil, fmt.Errorf("admission %s: observation %s must be independent and exclusively consumed", target.Name(), edge.From())
			}
			if edge.From() == target.Name() && members[edge.To()] {
				return nil, fmt.Errorf("admission %s: cycles are not yet supported", target.Name())
			}
		}
		groupName := "admission__" + target.Name()
		var incoming, outgoing []BetaEdge
		var external []externalBinding
		var in, out []string
		for _, edge := range p.Betas() {
			if members[edge.To()] && !members[edge.From()] {
				slot := fmt.Sprintf("__admission_input_%d", len(external))
				external = append(external, externalBinding{source: edge.Src(), slot: slot})
				in = append(in, slot)
				incoming = append(incoming, NewBetaEdge(edge.From(), groupName, edge.Src(), slot))
			} else if members[edge.From()] && !members[edge.To()] {
				outgoing = append(outgoing, NewBetaEdge(groupName, edge.To(), edge.Src(), edge.Tgt()))
			}
		}
		// Direct kappa inputs need binding too, even when they have no explicit edge.
		for _, name := range slices.Sorted(maps.Keys(members)) {
			for _, attr := range byName[name].In() {
				wired := false
				for _, edge := range p.Betas() {
					if edge.To() == name && edge.Tgt() == attr {
						wired = true
						break
					}
				}
				if !wired {
					slot := fmt.Sprintf("__admission_input_%d", len(external))
					external = append(external, externalBinding{source: attr, slot: slot})
					in = append(in, slot)
					incoming = append(incoming, NewBetaEdge("", groupName, attr, slot))
				}
			}
			out = append(out, byName[name].Out()...)
		}
		original := p
		group := WithInternal(NewExchangeSpec(groupName, in, out, func(bound map[string]any) facade.Operator {
			base := map[string]any{}
			for _, binding := range external {
				base[binding.source] = bound[binding.slot]
			}
			return &admissionGroup{plan: original, sources: sources, target: target, base: base}
		}, bind.NewTupleFlatten()))
		var exchanges []ExchangeSpec
		for _, spec := range p.Exchanges() {
			if !members[spec.Name()] {
				exchanges = append(exchanges, spec)
			}
		}
		exchanges = append(exchanges, coordinatorSpec{group})
		betas := append(incoming, outgoing...)
		for _, edge := range p.Betas() {
			if !members[edge.From()] && !members[edge.To()] {
				betas = append(betas, edge)
			}
		}
		var alphas []AlphaEdge
		for _, edge := range p.Alphas() {
			from, to := edge.From(), edge.To()
			if members[from] && members[to] {
				continue
			}
			if members[from] {
				from = groupName
			}
			if members[to] {
				to = groupName
			}
			alphas = append(alphas, NewAlphaEdge(from, to, edge.Alpha()))
		}
		p = admissionPlan{Plan: p, exchanges: exchanges, betas: betas, alphas: alphas}
	}
	return p, nil
}

type admissionGroup struct {
	plan    Plan
	sources []ExchangeSpec
	target  ExchangeSpec
	base    map[string]any
}
type observationEvent struct {
	source string
	row    map[string]any
	done   bool
	err    error
}

func (g *admissionGroup) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 16, 0)
	go func() { err := g.run(ctx, buf); buf.Complete(err) }()
	return buf.Reader()
}
func (g *admissionGroup) run(ctx context.Context, buf facade.Buffer) error {
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	byName := map[string]ExchangeSpec{}
	for _, spec := range g.plan.Exchanges() {
		byName[spec.Name()] = spec
	}
	accumulated := maps.Clone(g.base)
	states := map[string]facade.EdgeState{}
	sourceSet := map[string]bool{}
	for _, source := range g.sources {
		sourceSet[source.Name()] = true
	}
	update := func(source string, row map[string]any, done bool, failure error) {
		for _, edge := range g.plan.Betas() {
			if edge.To() != g.target.Name() || edge.From() != source {
				continue
			}
			state := states[edge.Tgt()]
			if value, present := row[edge.Src()]; present {
				state.Present = true
				state.Value = value
			}
			state.Done = done
			if failure != nil {
				state.Error = failure.Error()
			}
			states[edge.Tgt()] = state
		}
	}
	inbox := func() map[string]any {
		values := map[string]any{}
		for _, edge := range g.plan.Betas() {
			if edge.To() == g.target.Name() {
				if state := states[edge.Tgt()]; state.Present {
					values[edge.Tgt()] = state.Value
				}
			}
		}
		for _, attr := range g.target.In() {
			if _, present := values[attr]; present {
				continue
			}
			wired := false
			for _, binding := range g.plan.Betas() {
				if binding.To() == g.target.Name() && binding.Tgt() == attr {
					wired = true
					break
				}
			}
			if !wired {
				if value, present := accumulated[attr]; present {
					values[attr] = value
				}
			}
		}
		return values
	}
	for _, edge := range g.plan.Betas() {
		if edge.To() == g.target.Name() {
			if sourceSet[edge.From()] {
				states[edge.Tgt()] = facade.EdgeState{}
			} else {
				value, present := accumulated[edge.Src()]
				states[edge.Tgt()] = facade.EdgeState{Present: present, Value: value, Done: true}
			}
		}
	}
	transform := g.target.Inbound().(facade.InboxTransform)
	admit := func() (bool, error) { return transform.Admission().Admit(ctx, inbox(), states) }
	admitted, err := admit()
	if err != nil {
		return err
	}
	events := make(chan observationEvent)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	send := func(event observationEvent) bool {
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	if !admitted {
		for _, source := range g.sources {
			workers.Add(1)
			go func() {
				defer workers.Done()
				slot := &rowSlot{rec: bind.NewDocRecord(g.base)}
				id := int64(0)
				op, err := stage(g.plan, byName, nil, nil, component{members: []string{source.Name()}}, slot, &id)
				if err != nil {
					send(observationEvent{source: source.Name(), done: true, err: err})
					return
				}
				rows := op.Open(ctx)
				defer rows.Close()
				for rows.Next(ctx) {
					if row, ok := bind.DocMap(rows.Record()); ok {
						if !send(observationEvent{source: source.Name(), row: row}) {
							return
						}
					}
				}
				send(observationEvent{source: source.Name(), done: true, err: rows.Err()})
			}()
		}
		remaining := len(g.sources)
		for !admitted && remaining > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case event := <-events:
				if event.row != nil {
					maps.Copy(accumulated, event.row)
				}
				update(event.source, event.row, event.done, event.err)
				if event.done {
					remaining--
				}
				admitted, err = admit()
				if err != nil {
					return err
				}
			}
		}
	}
	if !admitted {
		return fmt.Errorf("admission %s: no sufficient observation after all reads ended", g.target.Name())
	}
	// Stop only alternative reads. The admitted action uses its parent's context,
	// so cancelling the losing read cannot cancel a mutation in flight.
	cancel()
	return g.consume(parent, accumulated, states, byName, buf)
}
func (g *admissionGroup) consume(ctx context.Context, row map[string]any, states map[string]facade.EdgeState, byName map[string]ExchangeSpec, buf facade.Buffer) error {
	id := int64(0)
	op, err := stage(g.plan, byName, nil, nil, component{members: []string{g.target.Name()}}, seedOp{rec: bind.NewDocRecord(row)}, &id)
	if err != nil {
		return err
	}
	rows := op.Open(bind.WithEdgeStates(ctx, states))
	defer rows.Close()
	for rows.Next(ctx) {
		if err := buf.Append(ctx, rows.Record()); err != nil {
			return err
		}
	}
	return rows.Err()
}

type coordinatorSpec struct{ ExchangeSpec }

func (coordinatorSpec) Coordinator() bool { return true }
