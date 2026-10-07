package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/buffer"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/system_g/scc"
	"github.com/stackql-labs/omnisdk/internal/system_g/schedule"
	"github.com/stackql-labs/omnisdk/internal/system_g/termination"
)

// component is one node of the condensation: a single exchange, or the members of a strongly
// connected component. cyclic is true for a component that loops — several members, or one fed by
// itself.
type component struct {
	members []string
	cyclic  bool
}

// condense is Tarjan's condensation of the exchanges over their β dependencies, ordered so every
// component follows the components it reads from (Kahn's, declaration order breaking ties). Members
// of a component keep declaration order. Edges from a κ input (From "") are bindings, not
// dependencies.
func condense(exchanges []ExchangeSpec, betas []BetaEdge) []component {
	var names []string
	pos := map[string]int{}
	for _, x := range exchanges {
		if _, ok := pos[x.Name()]; !ok {
			pos[x.Name()] = len(names)
			names = append(names, x.Name())
		}
	}
	adj := map[string][]string{}
	self := map[string]bool{}
	for _, e := range betas {
		if _, ok := pos[e.From()]; !ok {
			continue
		}
		if _, ok := pos[e.To()]; !ok {
			continue
		}
		if e.From() == e.To() {
			self[e.From()] = true
		}
		if !slices.Contains(adj[e.From()], e.To()) {
			adj[e.From()] = append(adj[e.From()], e.To())
		}
	}

	// Tarjan.
	index, low := map[string]int{}, map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var comps []component
	of := map[string]int{}
	next := 0
	var strong func(v string)
	strong = func(v string) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adj[v] {
			if _, seen := index[w]; !seen {
				strong(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] != index[v] {
			return
		}
		var members []string
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			members = append(members, w)
			if w == v {
				break
			}
		}
		slices.SortFunc(members, func(a, b string) int { return pos[a] - pos[b] })
		for _, m := range members {
			of[m] = len(comps)
		}
		comps = append(comps, component{members: members, cyclic: len(members) > 1 || self[v]})
	}
	for _, n := range names {
		if _, seen := index[n]; !seen {
			strong(n)
		}
	}

	// Kahn over the condensation, a component ready once every component feeding it has run.
	waiting := make([]int, len(comps))
	after := map[int][]int{}
	for from, tos := range adj {
		for _, to := range tos {
			a, b := of[from], of[to]
			if a != b && !slices.Contains(after[a], b) {
				after[a] = append(after[a], b)
				waiting[b]++
			}
		}
	}
	first := func(c int) int { return pos[comps[c].members[0]] }
	var ready []int
	for c := range comps {
		if waiting[c] == 0 {
			ready = append(ready, c)
		}
	}
	slices.SortFunc(ready, func(a, b int) int { return first(a) - first(b) })
	out := make([]component, 0, len(comps))
	for len(ready) > 0 {
		c := ready[0]
		ready = ready[1:]
		out = append(out, comps[c])
		for _, n := range after[c] {
			if waiting[n]--; waiting[n] == 0 {
				ready = append(ready, n)
			}
		}
	}
	return out
}

// cycleSpec is the termination a cycle runs under: the spec of each member that names one, the
// first bound reached winning. It is refused unless well-founded — a cycle that might never stop is
// rejected before any of it runs.
func cycleSpec(p Plan, c component) (facade.TerminationSpec, error) {
	var specs []facade.TerminationSpec
	for _, m := range c.members {
		if s, ok := p.Terminations()[m]; ok && s != nil {
			specs = append(specs, s)
		}
	}
	through := strings.Join(c.members, ", ")
	if len(specs) == 0 {
		return nil, fmt.Errorf("plan: cycle through %s has no termination policy; a loop needs a bound on rounds, records or time", through)
	}
	spec := termination.AnyOf(specs...)
	if err := spec.WellFounded(); err != nil {
		return nil, fmt.Errorf("plan: cycle through %s: %w", through, err)
	}
	return spec, nil
}

// CheckCycles proves every cycle of p can start and must stop, without running anything: each has a
// well-founded termination and an entry. It is what Compose checks first, exposed so a planner can
// refuse a plan when it is made rather than when it is opened.
func CheckCycles(p Plan) error {
	byName := make(map[string]ExchangeSpec, len(p.Exchanges()))
	for _, x := range p.Exchanges() {
		byName[x.Name()] = x
	}
	for _, c := range condense(p.Exchanges(), p.Betas()) {
		if !c.cyclic {
			continue
		}
		spec, err := cycleSpec(p, c)
		if err != nil {
			return err
		}
		if _, err := newCycleStage(p, byName, c, nil, spec); err != nil {
			return err
		}
	}
	return nil
}

// memberKey tags a record inside a cycle with the member that produced it, so the next round knows
// which edges it travels. It never leaves the cycle.
const memberKey = "\x00scc.member"

// cycleStage runs a condensed cycle once per upstream row. The members a value outside the cycle can
// start — entries — run first, on the row; then each round sends every new record along the cycle's
// own edges to the members they feed, until a round derives nothing new or the termination policy
// stops it. Every record reached leaves the stage, each one the upstream row with that member's
// output merged in.
type cycleStage struct {
	upstream facade.Operator
	members  map[string]ExchangeSpec
	order    []string
	entries  []string
	edges    map[string][]string // member → members it feeds within the cycle
	in       map[string]bool     // the cycle's members
	betas    []BetaEdge
	alphas   []AlphaEdge
	spec     facade.TerminationSpec
}

// newCycleStage builds the stage for c. An entry is a member whose every input can be had from
// outside the cycle — a β edge from another component, or a κ input by name — so it can run before
// any member has. A cycle with none is refused: nothing in it could ever start.
func newCycleStage(p Plan, byName map[string]ExchangeSpec, c component, upstream facade.Operator, spec facade.TerminationSpec) (facade.Operator, error) {
	in := map[string]bool{}
	for _, m := range c.members {
		in[m] = true
	}
	s := &cycleStage{upstream: upstream, members: map[string]ExchangeSpec{}, order: c.members,
		edges: map[string][]string{}, in: in, betas: p.Betas(), alphas: p.Alphas(), spec: spec}
	for _, e := range p.Betas() {
		if in[e.From()] && in[e.To()] && !slices.Contains(s.edges[e.From()], e.To()) {
			s.edges[e.From()] = append(s.edges[e.From()], e.To())
		}
	}
	for _, m := range c.members {
		s.members[m] = byName[m]
		if s.startable(m) {
			s.entries = append(s.entries, m)
		}
	}
	if len(s.entries) == 0 {
		return nil, fmt.Errorf("plan: cycle through %s has no entry: every member needs a value only another member supplies", strings.Join(c.members, ", "))
	}
	return s, nil
}

// startable reports whether every input of m can be had from outside the cycle.
func (s *cycleStage) startable(m string) bool {
	for _, attr := range s.members[m].In() {
		fromCycle, fromOutside := false, false
		for _, e := range s.betas {
			if e.To() != m || e.Tgt() != attr {
				continue
			}
			if s.in[e.From()] {
				fromCycle = true
			} else {
				fromOutside = true
			}
		}
		if fromCycle && !fromOutside {
			return false
		}
	}
	return true
}

// value is m's input attr on base when its run was caused by from ("" for an entry's first run): the
// edge from that member, else the first edge from outside the cycle carrying a value, else a κ input
// of the same name.
func (s *cycleStage) value(base map[string]any, m, attr, from string) any {
	var outside []string
	for _, e := range s.betas {
		if e.To() != m || e.Tgt() != attr {
			continue
		}
		if from != "" && e.From() == from {
			return base[e.Src()]
		}
		if !s.in[e.From()] {
			outside = append(outside, e.Src())
		}
	}
	for _, src := range outside {
		if v := base[src]; !emptyValue(v) {
			return v
		}
	}
	if len(outside) > 0 {
		return base[outside[0]]
	}
	return base[attr]
}

func (s *cycleStage) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, 1024, buffer.RowsAhead(ctx))
	go func() {
		var cerr error
		defer func() { buf.Complete(cerr) }()
		up := s.upstream.Open(ctx)
		defer up.Close()
		for up.Next(ctx) {
			row, ok := bind.DocMap(up.Record())
			if !ok {
				continue
			}
			if err := s.loop(ctx, row, func(r facade.Record) error { return buf.Append(ctx, r) }); err != nil {
				if !errors.Is(err, buffer.ErrAllReadersClosed) {
					cerr = err
				}
				return
			}
		}
		cerr = up.Err()
	}()
	return buf.Reader()
}

// loop runs the cycle for one upstream row.
func (s *cycleStage) loop(ctx context.Context, row map[string]any, emit func(facade.Record) error) error {
	var seed []facade.Record
	for _, m := range s.entries {
		recs, err := s.invoke(ctx, m, row, "")
		if err != nil {
			return err
		}
		seed = append(seed, recs...)
	}
	op := scc.NewSCC(0, staticRecords(seed), roundStep{s}, recordKey, s.spec.Policy(time.Now()), 1)
	out := op.Open(ctx)
	defer out.Close()
	emitted := false
	for out.Next(ctx) {
		m, ok := bind.DocMap(out.Record())
		if !ok {
			continue
		}
		clean := make(map[string]any, len(m))
		for k, v := range m {
			if k != memberKey {
				clean[k] = v
			}
		}
		if err := emit(bind.NewDocRecord(clean)); err != nil {
			return err
		}
		emitted = true
	}
	if err := out.Err(); err != nil {
		return err
	}
	if !emitted {
		// As for any stage: an upstream row the cycle reaches nothing from passes through or is
		// dropped, as the entry's flatten decides.
		rec, err := flattenOf(s.members[s.entries[0]]).Apply(bind.NewDocRecord(map[string]any{bind.KeyInput: row, bind.KeyOutput: nil}))
		if err != nil {
			return err
		}
		if rec != nil {
			return emit(rec)
		}
	}
	return nil
}

// invoke runs member m once on base, caused by from ("" for an entry's first run): its inputs bound
// from base along its β edges, reshaped by its T_in, after any α delay into it. Each result is base
// with m's output merged in, tagged with m.
func (s *cycleStage) invoke(ctx context.Context, m string, base map[string]any, from string) ([]facade.Record, error) {
	x := s.members[m]
	bound := make(map[string]any, len(x.In()))
	for _, attr := range x.In() {
		bound[attr] = s.value(base, m, attr, from)
	}
	if t := x.Inbound(); t != nil {
		shaped, err := bind.ApplyInbound(t, bound)
		if err != nil {
			return nil, err
		}
		bound = shaped
	}
	if a := alphaInto(s.alphas, m); a != nil && a.Delay() > 0 {
		t := time.NewTimer(a.Delay())
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		}
	}
	in := x.Make(bound).Open(ctx)
	defer in.Close()
	flat := flattenOf(x)
	var out []facade.Record
	for in.Next(ctx) {
		o, ok := bind.DocMap(in.Record())
		if !ok {
			continue
		}
		rec, err := flat.Apply(bind.NewDocRecord(map[string]any{bind.KeyInput: base, bind.KeyOutput: o}))
		if err != nil {
			return nil, err
		}
		merged, ok := bind.DocMap(rec)
		if !ok || merged == nil {
			continue
		}
		merged[memberKey] = m
		out = append(out, bind.NewDocRecord(merged))
	}
	return out, in.Err()
}

func flattenOf(x ExchangeSpec) facade.Transform {
	if f := x.Flatten(); f != nil {
		return f
	}
	return bind.NewTupleFlatten()
}

// roundStep is one round: every record of the frontier travels each cycle edge out of the member that
// produced it. Invocations within a round are independent and run concurrently, under the run's
// fan-out limit.
type roundStep struct{ s *cycleStage }

func (r roundStep) Round(ctx context.Context, frontier []facade.Record) ([]facade.Record, error) {
	lim := schedule.LimiterFrom(ctx)
	var mu sync.Mutex
	var derived []facade.Record
	var errs []error
	var wg sync.WaitGroup
	for _, rec := range frontier {
		m, ok := bind.DocMap(rec)
		if !ok {
			continue
		}
		from, _ := m[memberKey].(string)
		for _, to := range r.s.edges[from] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if lim != nil {
					tok, err := lim.Acquire(ctx)
					if err != nil {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
						return
					}
					defer tok.Release()
				}
				recs, err := r.s.invoke(ctx, to, m, from)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err)
					return
				}
				derived = append(derived, recs...)
			}()
		}
	}
	wg.Wait()
	return derived, errors.Join(errs...)
}

// recordKey identifies a record for fixpoint detection: the member and everything it carries, so a
// member reaching the same result twice stops there.
func recordKey(r facade.Record) string {
	m, ok := bind.DocMap(r)
	if !ok {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprint(m)
	}
	return string(b)
}

// staticRecords is an operator over records already in hand.
type staticRecords []facade.Record

func (s staticRecords) Open(ctx context.Context) facade.Records {
	buf := buffer.NewBuffer(1, len(s)+1, 0)
	go func() {
		defer buf.Complete(nil)
		for _, r := range s {
			if err := buf.Append(ctx, r); err != nil {
				return
			}
		}
	}()
	return buf.Reader()
}
