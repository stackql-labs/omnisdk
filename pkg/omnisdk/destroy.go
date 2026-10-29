package omnisdk

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// destroyStatement removes everything a collection holds.
type destroyStatement struct{ keys []string }

func (destroyStatement) statement() {}

// Destroy removes the supplied resource keys, or every object when no keys are supplied.
// Selected deletion refuses resources with retained dependents before sending any delete.
// It needs the same ledger and explicit scope as full deletion.
//
// Full destruction is every object the collection's ledger holds, deleted. It needs NewLedger, whose
// collection it destroys: what each object is, how it is addressed and what depends on it come from
// the ledger, as the runs that made them recorded it.
//
// Dependents go first, so an object is deleted only after whatever reads from it. A delete the
// provider refuses is tried again once a pass has deleted something else — a dependent the ledger
// did not know of may have gone — and what still refuses when a pass deletes nothing is reported as
// outstanding with the provider's reason, never forced. Each delete is written to the run's journal
// before it is sent, and the key is forgotten once it lands.
func Destroy(keys ...string) Statement { return destroyStatement{keys: slices.Clone(keys)} }

type destroyPlan struct {
	registry, name, state, runID string
	args                         Args
	keys                         []string
}

func (p *destroyPlan) Open(ctx context.Context) (Rows, error) {
	var accessErr error
	ctx, accessErr = executionContext(ctx, p.args)
	if accessErr != nil {
		return nil, accessErr
	}
	if err := facade.CheckAccess(ctx, "collection/"+p.name, "destroy"); err != nil {
		return nil, err
	}
	h, err := hold(ctx, p.state, p.name, p.runID)
	if err != nil {
		return nil, err
	}
	defer h.release()
	entries, err := h.log.List(ctx, p.name+"/")
	if err != nil {
		return nil, err
	}

	selected := map[facade.LedgerKey]bool{}
	for _, key := range p.keys {
		if key == "" || strings.HasPrefix(key, ".") || strings.Contains(key, "..") {
			return nil, fmt.Errorf("omnisdk: invalid resource key %q", key)
		}
		selected[facade.LedgerKey(p.name+"/"+key)] = true
	}
	if len(selected) > 0 {
		found := map[facade.LedgerKey]bool{}
		for _, entry := range entries {
			if strings.HasPrefix(strings.TrimPrefix(string(entry.Key()), p.name+"/"), ".") {
				continue
			}
			found[entry.Key()] = true
			var rec aboutRecord
			if err := json.Unmarshal(entry.About(), &rec); err != nil || rec.Address == "" {
				return nil, fmt.Errorf("omnisdk: cannot check dependencies of %s", entry.Key())
			}
			if selected[entry.Key()] {
				continue
			}
			for _, dependency := range rec.DependsOn {
				if selected[facade.LedgerKey(dependency)] {
					return nil, fmt.Errorf("omnisdk: cannot delete %s: retained resource %s depends on it", dependency, entry.Key())
				}
			}
		}
		for _, key := range slices.Sorted(maps.Keys(selected)) {
			if !found[key] {
				return nil, fmt.Errorf("omnisdk: resource %s is not in the ledger", key)
			}
		}
	}

	type target struct {
		key      facade.LedgerKey
		about    aboutRecord
		identity []byte
	}
	var out []Row
	targets := map[facade.LedgerKey]target{}
	var resources []ManagedResource
	for _, e := range entries {
		// The collection's own bookkeeping — its lease — lives beside its keys and is not one.
		if strings.HasPrefix(strings.TrimPrefix(string(e.Key()), p.name+"/"), ".") {
			continue
		}
		if len(selected) > 0 && !selected[e.Key()] {
			continue
		}
		var rec aboutRecord
		if err := json.Unmarshal(e.About(), &rec); err != nil || rec.Address == "" {
			out = append(out, Row{"key": string(e.Key()), "status": "outstanding",
				"reason": "the ledger does not record what this is, so it cannot be addressed"})
			continue
		}
		if e.Phase() != facade.LedgerLive || len(e.Identity()) == 0 {
			out = append(out, Row{"key": string(e.Key()), "status": "outstanding",
				"reason": "never confirmed made: read the target before removing it by hand"})
			continue
		}
		targets[e.Key()] = target{key: e.Key(), about: rec, identity: e.Identity()}
		rel := strings.TrimPrefix(string(e.Key()), p.name+"/")
		resources = append(resources, NewResource(rel, rec.Provider, rec.Address, nil, rec.Params, nil, "", "", "", rec.AddressedBy, ""))
	}
	if len(targets) == 0 {
		return &sliceRows{rows: out, i: -1}, nil
	}
	effector, _, err := providerWiring(p.registry, resources, p.args)
	if err != nil {
		return nil, err
	}
	reg, err := openRegistry(p.registry)
	if err != nil {
		return nil, err
	}
	exchange := map[facade.LedgerKey]string{}
	for k, t := range targets {
		cat, err := reg.Catalog(t.about.Provider)
		if err != nil {
			return nil, err
		}
		addr, err := qualify(cat, t.about.Address)
		if err != nil {
			return nil, err
		}
		exchange[k] = exchangeOf(addr, "delete")
	}

	// Dependents first: a key is taken only once nothing left in the collection reads from it.
	pending := slices.Sorted(maps.Keys(targets))
	reasons := map[facade.LedgerKey]string{}
	for progress := true; progress && len(pending) > 0; {
		progress = false
		var next []facade.LedgerKey
		for _, k := range pending {
			if slices.ContainsFunc(pending, func(o facade.LedgerKey) bool {
				return o != k && slices.Contains(targets[o].about.DependsOn, string(k))
			}) {
				next = append(next, k)
				continue
			}
			if err := p.delete(ctx, h, k, exchange[k], targets[k].about, targets[k].identity, effector); err != nil {
				reasons[k] = err.Error()
				next = append(next, k)
				continue
			}
			out = append(out, Row{"key": string(k), "status": "destroyed", "identity": string(targets[k].identity)})
			delete(reasons, k)
			progress = true
		}
		pending = next
	}
	for _, k := range pending {
		reason := reasons[k]
		if reason == "" {
			reason = "something it depends on, or that depends on it, could not be removed"
		}
		out = append(out, Row{"key": string(k), "status": "outstanding", "reason": reason})
	}
	return &sliceRows{rows: out, i: -1}, nil
}

// delete removes one object: its intent to go into the journal first, the call, then the key.
func (p *destroyPlan) delete(ctx context.Context, h *held, k facade.LedgerKey, exchange string, rec aboutRecord,
	identity []byte, effector facade.Effector) error {
	if _, err := h.journal.Append(ctx, k, exchange, facade.FormDelete, nil); err != nil {
		return fmt.Errorf("recording the delete failed, so it was not sent: %w", err)
	}
	params := map[string]string{}
	maps.Copy(params, p.args.Params)
	maps.Copy(params, rec.inputs(ctx, h.log))
	if _, err := effector.Effect(ctx, exchange, k, facade.EffectInput{Params: params, Identity: identity}); err != nil {
		return err
	}
	_, v, found, err := h.log.Get(ctx, k)
	if err != nil || !found {
		return err
	}
	return h.log.Forget(ctx, k, v)
}
