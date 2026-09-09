// Package unwind compensates a failed run.
//
// Order is the reverse of the run's forward journal, but that order is only a heuristic: append
// order is intent order, concurrent appends are ordered arbitrarily, and the plan's data flow
// misses containment either way. Correctness therefore comes from the loop, not the ordering —
// attempt every compensation, treat a referential-integrity refusal as retryable, and repeat until
// a pass makes no progress. Letting the provider arbitrate is cheaper than modelling the
// dependencies it already enforces.
//
// Compensations are derived here from Plan and Identity rather than recorded at Begin, and each is
// written as a normal ledger entry before it is executed.
package unwind

import (
	"context"
	"errors"
	"fmt"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Outcome reports how far the loop got.
type Outcome struct {
	// Compensated is the keys reversed, in the order they were reversed.
	Compensated []facade.LedgerKey
	// Outstanding is what could not be reversed: blocked by the provider on every pass, or with
	// no inverse declared. A run ending with a non-empty Outstanding is partially compensated and
	// must say so rather than reporting success.
	Outstanding []facade.LedgerKey
	// Reasons explains each outstanding key.
	Reasons map[facade.LedgerKey]string
}

// Complete reports whether everything was reversed.
func (o Outcome) Complete() bool { return len(o.Outstanding) == 0 }

// Unwinder reverses a run.
type Unwinder interface {
	Unwind(ctx context.Context, runID string) (Outcome, error)
}

type unwinder struct {
	log       facade.Ledger
	journals  facade.Journals
	semantics facade.Semantics
	effect    facade.Effector
}

// New returns an Unwinder over the durable state of a run.
func New(log facade.Ledger, journals facade.Journals, semantics facade.Semantics, effect facade.Effector) Unwinder {
	return &unwinder{log: log, journals: journals, semantics: semantics, effect: effect}
}

// pending is one compensation still to attempt.
type pending struct {
	key      facade.LedgerKey
	exchange string
	form     facade.FormClass
	prior    []byte
}

// input assembles what the compensating call needs to address the object. Identity is the whole of
// it: a compensation is derived from Plan and Identity alone, which is why Identity must carry the
// full address rather than an id.
func input(e facade.LedgerEntry) facade.EffectInput {
	return facade.EffectInput{Identity: e.Identity(), Mutation: e.Plan()}
}

// restore re-sends the intent an update replaced, through the same exchange that performed it. The
// object survives — it was not created by this run — and its entry is left describing the intent
// that is now live again.
func (u *unwinder) restore(ctx context.Context, p pending) error {
	e, v, found, err := u.log.Get(ctx, p.key)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if len(p.prior) == 0 {
		return fmt.Errorf("update to %s recorded no prior intent to restore", p.key)
	}
	identity := e.Identity()
	if _, err := u.effect.Effect(ctx, p.exchange, p.key, facade.EffectInput{
		Mutation: p.prior,
		Identity: identity,
	}); err != nil {
		return err
	}
	// The entry is walked back the ordinary way: intent recorded, then resolved, so a restored key
	// is indistinguishable from one that was never touched.
	if err := u.log.Begin(ctx, p.key, p.prior, v); err != nil {
		return err
	}
	_, v, found, err = u.log.Get(ctx, p.key)
	if err != nil || !found {
		return err
	}
	return u.log.Resolve(ctx, p.key, identity, v)
}

func (u *unwinder) Unwind(ctx context.Context, runID string) (Outcome, error) {
	j, err := u.journals.For(ctx, runID)
	if err != nil {
		return Outcome{}, err
	}
	recs, err := j.Records(ctx)
	if err != nil {
		return Outcome{}, err
	}

	out := Outcome{Reasons: map[facade.LedgerKey]string{}}
	todo := make([]pending, 0, len(recs))
	for i := len(recs) - 1; i >= 0; i-- {
		todo = append(todo, pending{
			key:      recs[i].Key(),
			exchange: recs[i].Exchange(),
			form:     recs[i].Form(),
			prior:    recs[i].Prior(),
		})
	}

	// Fixpoint: each pass retries what the provider refused, because an earlier compensation in
	// the same pass may have removed the child that blocked it. Terminates when a pass reverses
	// nothing, which bounds the loop at one pass per remaining key.
	for len(todo) > 0 {
		var blocked []pending
		progressed := false
		for _, p := range todo {
			switch err := u.compensate(ctx, p); {
			case err == nil:
				out.Compensated = append(out.Compensated, p.key)
				progressed = true
			case errors.Is(err, facade.ErrCompensationBlocked):
				blocked = append(blocked, p)
				out.Reasons[p.key] = err.Error()
			default:
				out.Outstanding = append(out.Outstanding, p.key)
				out.Reasons[p.key] = err.Error()
			}
		}
		todo = blocked
		if !progressed {
			break
		}
	}
	for _, p := range todo {
		out.Outstanding = append(out.Outstanding, p.key)
	}
	return out, nil
}

// compensate derives and runs the inverse for one key. A compensation is a normal entry with its
// own Pending and Resolve: modelling it as a special case would mean retrofitting one later.
//
// The form decides the shape. Undoing a create is a delete; undoing an update is a restore of the
// prior intent, and deleting there would destroy an object an earlier run created rather than
// compensate this one.
func (u *unwinder) compensate(ctx context.Context, p pending) error {
	if p.form == facade.FormUpdate {
		return u.restore(ctx, p)
	}

	inv, ok := u.semantics.Inverse(p.exchange)
	if !ok || inv.Fidelity() == facade.FidelityNone {
		return fmt.Errorf("no inverse declared for %s", p.exchange)
	}

	e, v, found, err := u.log.Get(ctx, p.key)
	if err != nil {
		return err
	}
	if !found {
		// Nothing recorded, so nothing landed that we know of; there is nothing to reverse.
		return nil
	}

	// An unresolved entry is the dangerous case: the effect may or may not have landed, so the
	// standing rule applies — read the target, never re-execute or reverse blind.
	if e.Phase() == facade.LedgerPending {
		actual, _, readable, readErr := u.effect.Read(ctx, p.exchange, p.key, input(e))
		if readErr != nil {
			return readErr
		}
		if !readable {
			// No usable read, so whether the effect landed is unknown. Forgetting here would drop
			// the only record of a resource that may exist, leaving it orphaned and unmanaged —
			// this is the irreducible residue of a log-only identity, and it is reported rather
			// than guessed away.
			return fmt.Errorf("%s is pending and %s exposes no usable read: cannot tell whether the effect landed", p.key, p.exchange)
		}
		if len(actual) == 0 {
			// Read succeeded and the object is not there: the call never landed, so the intent is
			// all that exists and it can be dropped.
			return u.log.Forget(ctx, p.key, v)
		}
	}

	if _, err := u.effect.Effect(ctx, inv.Exchange(), p.key, input(e)); err != nil {
		return err
	}
	e, v, found, err = u.log.Get(ctx, p.key)
	if err != nil || !found {
		return err
	}
	return u.log.Forget(ctx, p.key, v)
}
