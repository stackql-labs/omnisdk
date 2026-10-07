// Package apply drives one IaC run: take the lease, record intent ahead of every effect, converge
// each key, and unwind on failure.
//
// It is a separate entry point rather than a change to the query executor: nothing here is reached
// by an existing query.
//
// Everything needed is a durable log and a readable target system. Nothing else is structural —
// the intent format is irrelevant to the argument, which is why a step carries opaque bytes and no
// package here parses them.
package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/stackql-labs/omnisdk/internal/system_g/bind"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
	"github.com/stackql-labs/omnisdk/internal/unwind"
)

// Step is one key's desired intent. Desired is opaque: the ledger stores it, the merge strategy
// reads it, and nothing else looks inside.
type Step struct {
	Key      facade.LedgerKey
	Exchange string
	Desired  []byte
	// Params address the object: the parameters that name it on the wire.
	Params map[string]string
	// Inbound is what arrives from other keys: each names a sibling whose recorded identity lands in
	// this step's inbox under a chosen name. It is the dependency a β edge carries inside a single
	// plan, carried through the ledger instead because each effect is wrapped in its own durable
	// writes. A key named here must already be live.
	Inbound []Arrival
	// Via reshapes the inbox into this step's parameters — T_in, the same contract a query uses.
	//
	// It exists for the same reason: a value in the shape one provider emits is frequently not the
	// shape the next call accepts, and an inbox holding several arrivals may yield one parameter
	// built from all of them. Nil is identity, which is the common case.
	Via facade.Transform
}

// Arrival is one value reaching a step from a sibling key.
type Arrival struct {
	// From is the sibling key whose identity is delivered.
	From facade.LedgerKey
	// As is the name it takes in the inbox; empty means the sibling's key.
	As string
}

// Result reports what a run did. A run that failed and could not fully compensate is partially
// compensated, and says so rather than reporting either success or a clean rollback.
type Result struct {
	Applied []facade.LedgerKey
	Failed  facade.LedgerKey
	Err     error
	Unwound *unwind.Outcome
}

// Complete reports whether every step landed.
func (r Result) Complete() bool { return r.Err == nil }

// Runner applies a set of steps under a lease.
type Runner interface {
	Apply(ctx context.Context, runID, scope string, steps []Step) (Result, error)
}

type runner struct {
	log       facade.Ledger
	journals  facade.Journals
	leaser    facade.Leaser
	merge     facade.Merge
	effect    facade.Effector
	semantics facade.Semantics
	unwinder  unwind.Unwinder
	keys      facade.KeySet
	ttl       time.Duration
	// parallelism bounds how many keys converge at once.
	parallelism int
}

// DefaultParallelism is how many keys converge at once when the caller does not say.
const DefaultParallelism = 16

// New wires a Runner. keys is the lease's key set — v1 passes everything, and narrowing it later
// is configuration rather than a rewrite. parallelism bounds how many keys converge at once; zero or
// less is DefaultParallelism.
func New(
	log facade.Ledger,
	journals facade.Journals,
	leaser facade.Leaser,
	m facade.Merge,
	effect facade.Effector,
	sem facade.Semantics,
	u unwind.Unwinder,
	keys facade.KeySet,
	ttl time.Duration,
	parallelism int,
) Runner {
	if parallelism <= 0 {
		parallelism = DefaultParallelism
	}
	return &runner{log: log, journals: journals, leaser: leaser, merge: m, effect: effect, semantics: sem, unwinder: u,
		keys: keys, ttl: ttl, parallelism: parallelism}
}

func (r *runner) Apply(ctx context.Context, runID, scope string, steps []Step) (Result, error) {
	deps, err := dependencies(steps)
	if err != nil {
		return Result{}, err
	}
	// The candidate set is declared before planning, so the lease is taken up front. Read-validate
	// -write would be correct too, but optimistic validation starves long transactions, and an IaC
	// apply is a long transaction.
	lease, err := r.leaser.Acquire(ctx, scope, r.keys, runID, r.ttl)
	if err != nil {
		return Result{}, fmt.Errorf("apply: lease %s: %w", scope, err)
	}
	defer func() { _ = lease.Release(ctx) }()

	j, err := r.journals.For(ctx, runID)
	if err != nil {
		return Result{}, err
	}

	res := r.converge(ctx, j, steps, deps)
	if res.Err == nil {
		return res, nil
	}
	// Forward recovery is preferred where it is reachable, because compensation is lossy — billing
	// events, sent notifications, consumed ids and deleted data do not come back. Choosing between
	// them is the caller's policy, so the outcome is reported rather than decided here.
	out, unwindErr := r.unwinder.Unwind(ctx, runID)
	if unwindErr != nil {
		return res, errors.Join(res.Err, unwindErr)
	}
	res.Unwound = &out
	return res, nil
}

// converge runs every step once the keys it reads from are live, up to parallelism at a time. A key
// with nothing between it and another runs alongside it: a run waits on its dependencies, not on its
// declaration order.
//
// The first failure stops new steps from starting. Steps already in flight finish rather than being
// cancelled — an effect abandoned mid-call has an unknown outcome — and everything that landed is then
// in the journal for the unwind.
func (r *runner) converge(ctx context.Context, j facade.Journal, steps []Step, deps map[int][]int) Result {
	waiting := make([]int, len(steps)) // unfinished dependencies per step
	after := make(map[int][]int)       // steps that depend on each step
	var ready []int
	for i := range steps {
		waiting[i] = len(deps[i])
		for _, d := range deps[i] {
			after[d] = append(after[d], i)
		}
		if waiting[i] == 0 {
			ready = append(ready, i)
		}
	}

	type done struct {
		i   int
		err error
	}
	finished := make(chan done)
	var res Result
	var errs []error
	running := 0
	for len(ready) > 0 || running > 0 {
		for len(ready) > 0 && running < r.parallelism && res.Err == nil && len(errs) == 0 {
			i := ready[0]
			ready = ready[1:]
			running++
			go func() { finished <- done{i, r.step(ctx, j, steps[i])} }()
		}
		if running == 0 {
			break // a failure has stopped new steps, and nothing is left in flight
		}
		d := <-finished
		running--
		if d.err != nil {
			if len(errs) == 0 {
				res.Failed = steps[d.i].Key
			}
			errs = append(errs, fmt.Errorf("%s: %w", steps[d.i].Key, d.err))
			continue
		}
		res.Applied = append(res.Applied, steps[d.i].Key)
		for _, n := range after[d.i] {
			if waiting[n]--; waiting[n] == 0 {
				ready = append(ready, n)
			}
		}
	}
	if len(errs) == 1 {
		res.Err = errors.Unwrap(errs[0])
	} else if len(errs) > 1 {
		res.Err = errors.Join(errs...)
	}
	return res
}

// dependencies maps each step to the steps whose keys it reads from. An arrival from a key outside
// the run is not a dependency: it must already be live, which bind checks. A cycle is refused before
// anything is taken or sent.
func dependencies(steps []Step) (map[int][]int, error) {
	index := make(map[facade.LedgerKey]int, len(steps))
	for i, s := range steps {
		if _, dup := index[s.Key]; dup {
			return nil, fmt.Errorf("apply: %s appears twice", s.Key)
		}
		index[s.Key] = i
	}
	deps := make(map[int][]int, len(steps))
	for i, s := range steps {
		for _, in := range s.Inbound {
			if d, ok := index[in.From]; ok && !slices.Contains(deps[i], d) {
				deps[i] = append(deps[i], d)
			}
		}
	}
	// Kahn's: whatever never becomes ready is on a cycle.
	waiting := make([]int, len(steps))
	after := map[int][]int{}
	var queue []int
	for i := range steps {
		waiting[i] = len(deps[i])
		for _, d := range deps[i] {
			after[d] = append(after[d], i)
		}
		if waiting[i] == 0 {
			queue = append(queue, i)
		}
	}
	seen := 0
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		seen++
		for _, n := range after[i] {
			if waiting[n]--; waiting[n] == 0 {
				queue = append(queue, n)
			}
		}
	}
	if seen != len(steps) {
		var cyclic []string
		for i, w := range waiting {
			if w > 0 {
				cyclic = append(cyclic, string(steps[i].Key))
			}
		}
		return nil, fmt.Errorf("apply: dependency cycle among %s", strings.Join(cyclic, ", "))
	}
	return deps, nil
}

// adopt writes an entry for an object the target already holds. Intent and identity are recorded in
// the ordinary two steps, so an adopted key is indistinguishable from one this run created.
func (r *runner) adopt(ctx context.Context, s Step, v facade.LedgerVersion, identity []byte) error {
	if err := r.log.Begin(ctx, s.Key, s.Desired, v); err != nil {
		return err
	}
	_, v, found, err := r.log.Get(ctx, s.Key)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("apply: %s vanished during adoption", s.Key)
	}
	return r.log.Resolve(ctx, s.Key, identity, v)
}

// Satisfied reports whether actual already satisfies mutation: every enforced field matches, and every
// unset field is absent. It is the test for "nothing to do".
func Satisfied(mutation, actual []byte) (bool, error) { return converged(mutation, actual) }

// converged reports whether actual already satisfies mutation: every enforced field matches, and
// every unset field is absent. It is the test for "nothing to do", and it is why a re-run costs no
// wire calls.
func converged(mutation, actual []byte) (bool, error) {
	want, err := decodeDoc(mutation)
	if err != nil {
		return false, fmt.Errorf("apply: mutation is not a JSON object: %w", err)
	}
	have, err := decodeDoc(actual)
	if err != nil {
		// An unreadable actual is not a match; converging on a target we cannot read is the case
		// the design already calls belief rather than fact.
		return false, nil
	}
	return satisfied(want, have), nil
}

// decodeDoc reads a document keeping numbers as their source text, so comparison is between what
// was written and what came back rather than between two float64 approximations of them.
func decodeDoc(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var o map[string]any
	if err := dec.Decode(&o); err != nil {
		return nil, err
	}
	return o, nil
}

func satisfied(want, have map[string]any) bool {
	for k, v := range want {
		got, present := have[k]
		if v == nil {
			if present {
				return false
			}
			continue
		}
		if !present {
			return false
		}
		wsub, wok := v.(map[string]any)
		gsub, gok := got.(map[string]any)
		if wok && gok {
			if !satisfied(wsub, gsub) {
				return false
			}
			continue
		}
		if fmt.Sprint(v) != fmt.Sprint(got) {
			return false
		}
	}
	return true
}

// bind assembles a step's parameters: what the caller supplied, plus what arrives from sibling keys,
// reshaped by T_in where one is declared.
//
// An arrival from a key that is not live is an error rather than a silent empty string: it means the
// dependency was ordered wrongly, and sending an unbound parameter would create an orphan.
func (r *runner) bind(ctx context.Context, s Step) (map[string]string, error) {
	inbox := make(map[string]any, len(s.Inbound))
	for _, in := range s.Inbound {
		e, _, found, err := r.log.Get(ctx, in.From)
		if err != nil {
			return nil, err
		}
		if !found || e.Phase() != facade.LedgerLive {
			return nil, fmt.Errorf("apply: %s expects a value from %s, which is not live", s.Key, in.From)
		}
		name := in.As
		if name == "" {
			name = string(in.From)
		}
		inbox[name] = string(e.Identity())
	}

	params := make(map[string]string, len(s.Params)+len(inbox))
	maps.Copy(params, s.Params)
	if s.Via == nil {
		// Identity: the inbox IS the parameters, by name.
		for k, v := range inbox {
			params[k] = fmt.Sprint(v)
		}
		return params, nil
	}
	shaped, err := s.Via.Apply(bind.NewDocRecord(inbox))
	if err != nil {
		return nil, fmt.Errorf("apply: %s inbound transform: %w", s.Key, err)
	}
	out, ok := bind.DocMap(shaped)
	if !ok {
		return nil, fmt.Errorf("apply: %s inbound transform did not yield a document", s.Key)
	}
	// The transform's output overrides what the caller supplied: it is the more specific statement,
	// built from values only known at run time.
	for k, v := range out {
		params[k] = fmt.Sprint(v)
	}
	return params, nil
}

// step converges one key. Order is load-bearing: the journal and the ledger both record intent
// before the wire call, so a crash can never leave an effect that nothing knows to ask about.
func (r *runner) step(ctx context.Context, j facade.Journal, s Step) error {
	e, v, found, err := r.log.Get(ctx, s.Key)
	if err != nil {
		return err
	}
	var prior []byte
	if found {
		prior = e.Plan()
	} else {
		v = facade.LedgerVersionNone
	}

	// Actual is read live and never persisted as the merge basis. Where a target exposes no usable
	// read the log degrades from fact to belief — a property of the target, not of the design — and
	// convergence weakens to idempotence.
	params, err := r.bind(ctx, s)
	if err != nil {
		return err
	}

	var identity []byte
	if found {
		identity = e.Identity()
	}
	// The exchange a step names is its create; convergence may dispatch elsewhere below. The form
	// travels with it, because it decides whether compensating means deleting or restoring.
	exchange, form := s.Exchange, facade.FormCreate
	actual, discovered, _, err := r.effect.Read(ctx, s.Exchange, s.Key, facade.EffectInput{Params: params, Identity: identity})
	if err != nil {
		return err
	}

	// Adoption: the target holds an object bearing this key's correlation stamp that the ledger does
	// not know about — an entry lost, or a run that died between the effect and its Resolve. Take
	// it over rather than creating a second one. This is what keeps the store a cache: lose it and
	// the objects are still found by enumerating the scope.
	adopted := len(identity) == 0 && len(discovered) > 0
	if adopted {
		identity = discovered
	}

	mutation, err := r.merge.Apply(prior, s.Desired, actual)
	if err != nil {
		return err
	}

	// Convergence, not replay: a live entry whose target already satisfies the mutation needs no
	// call at all. Without this a re-apply re-issues the create, because the exchange named by a
	// step is fixed and nothing else distinguishes "make this" from "keep this".
	if (adopted || (found && e.Phase() == facade.LedgerLive)) && len(actual) > 0 {
		same, err := converged(mutation, actual)
		if err != nil {
			return err
		}
		if same {
			if adopted {
				// Record what the target already holds, so the next run needs no rediscovery.
				return r.adopt(ctx, s, v, identity)
			}
			return nil
		}
		// The object exists and does not match. Which exchange converges it is declared, not
		// guessed: re-issuing a create is correct for an upsert and mints a duplicate otherwise.
		up, ok := r.semantics.Update(s.Exchange)
		if !ok {
			return fmt.Errorf("apply: %s has drifted and %s declares no update path", s.Key, s.Exchange)
		}
		exchange, form = up, facade.FormUpdate
	}

	// The journal is appended here rather than on entry, once a wire effect is certain to be
	// attempted. It is still write-ahead — nothing has been sent — but a step that converged or was
	// refused leaves no record, so unwinding cannot compensate an effect this run never caused.
	// prior is journalled only for an update: it is what the compensation restores, and Resolve is
	// about to promote the new intent over it, after which the entry no longer carries it.
	var undo []byte
	if form == facade.FormUpdate {
		undo = prior
	}
	if _, err := j.Append(ctx, s.Key, exchange, form, undo); err != nil {
		return err
	}

	// Intent is recorded, not the mutation: the mutation is derived, and persisting a derived
	// value would put a stale basis where the resolved intent belongs.
	if err := r.log.Begin(ctx, s.Key, s.Desired, v); err != nil {
		return err
	}

	identity, err = r.effect.Effect(ctx, exchange, s.Key, facade.EffectInput{Params: params, Mutation: mutation, Identity: identity})
	if err != nil {
		return err
	}

	_, v, found, err = r.log.Get(ctx, s.Key)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("apply: %s vanished between begin and resolve", s.Key)
	}
	return r.log.Resolve(ctx, s.Key, identity, v)
}
