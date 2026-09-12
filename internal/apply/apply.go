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
	"time"

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
	// Bindings resolve a parameter from another key's recorded identity — the dependency a β edge
	// carries inside a single plan, carried through the ledger instead because each effect is
	// wrapped in its own durable writes. The bound key must already be live.
	Bindings map[string]facade.LedgerKey
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
}

// New wires a Runner. keys is the lease's key set — v1 passes everything, and narrowing it later
// is configuration rather than a rewrite.
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
) Runner {
	return &runner{log: log, journals: journals, leaser: leaser, merge: m, effect: effect, semantics: sem, unwinder: u, keys: keys, ttl: ttl}
}

func (r *runner) Apply(ctx context.Context, runID, scope string, steps []Step) (Result, error) {
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

	var res Result
	for _, s := range steps {
		if err := r.step(ctx, j, s); err != nil {
			res.Failed, res.Err = s.Key, err
			// Forward recovery is preferred where it is reachable, because compensation is lossy —
			// billing events, sent notifications, consumed ids and deleted data do not come back.
			// Choosing between them is the caller's policy, so the outcome is reported rather than
			// decided here.
			out, unwindErr := r.unwinder.Unwind(ctx, runID)
			if unwindErr != nil {
				return res, errors.Join(err, unwindErr)
			}
			res.Unwound = &out
			return res, nil
		}
		res.Applied = append(res.Applied, s.Key)
	}
	return res, nil
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

// bind resolves a step's parameters, taking bound ones from the recorded identity of another key.
// A binding onto a key that is not live is an error rather than a silent empty string: it means the
// dependency was ordered wrongly, and sending an unbound parameter would create an orphan.
func (r *runner) bind(ctx context.Context, s Step) (map[string]string, error) {
	params := make(map[string]string, len(s.Params)+len(s.Bindings))
	maps.Copy(params, s.Params)
	for param, src := range s.Bindings {
		e, _, found, err := r.log.Get(ctx, src)
		if err != nil {
			return nil, err
		}
		if !found || e.Phase() != facade.LedgerLive {
			return nil, fmt.Errorf("apply: %s binds %s from %s, which is not live", s.Key, param, src)
		}
		params[param] = string(e.Identity())
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
