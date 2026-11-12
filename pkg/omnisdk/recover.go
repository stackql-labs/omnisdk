package omnisdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/journal"
	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Recover explicitly records an operator-verified outcome for a pending resource.
// It never calls a provider. The observed ledger version is required to reject stale decisions.
// absent permits another create only when there is no prior identity or dependent resource.
// applied requires the identity of the verified completed effect. Neither choice infers an outcome.
func (w ledgerWAL) Recover(ctx context.Context, resource, outcome, identity, version string, args Args) (result []Row, resultErr error) {
	if err := checkWAL(w); err != nil {
		return nil, err
	}
	if resource == "" || !filepath.IsLocal(resource) || strings.Contains(resource, "\\") || strings.HasPrefix(resource, ".") || version == "" {
		return nil, fmt.Errorf("recovery requires a resource key and observed ledger version")
	}
	if outcome != "absent" && outcome != "applied" {
		return nil, fmt.Errorf("recovery outcome must be absent or applied, verified by the operator")
	}
	if outcome == "absent" && identity != "" || outcome == "applied" && identity == "" {
		return nil, fmt.Errorf("only applied recovery requires an identity")
	}
	ctx, resultErr = authorizeExecution(ctx, args, facade.Permission{Resource: "collection/" + w.collection, Action: "recover"})
	if resultErr != nil {
		return nil, resultErr
	}
	key := facade.LedgerKey(w.collection + "/" + resource)
	h, err := hold(ctx, w.state, w.collection, w.runID, lease.Of(key))
	if err != nil {
		return nil, err
	}
	defer h.release()
	ctx = h.ctx
	defer func() {
		if resultErr != nil {
			resultErr = recordRunFailure(ctx, h, key, resultErr)
		}
	}()
	entry, current, found, err := h.log.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found || entry.Phase() != facade.LedgerPending {
		return nil, fmt.Errorf("%s is not pending", key)
	}
	if string(current) != version {
		return nil, fmt.Errorf("%w: recovery observed %s, current %s", facade.ErrLedgerConflict, version, current)
	}
	if outcome == "absent" {
		if len(entry.Identity()) != 0 || len(entry.Plan()) != 0 {
			return nil, fmt.Errorf("absent recovery only clears an unresolved first create; prior state must be preserved")
		}
		entries, err := h.log.List(ctx, w.collection+"/")
		if err != nil {
			return nil, err
		}
		for _, other := range entries {
			if other.Key() == key || strings.HasPrefix(strings.TrimPrefix(string(other.Key()), w.collection+"/"), ".") {
				continue
			}
			metadata := other.About()
			if len(metadata) == 0 {
				metadata, err = journal.RecoveryAbout(ctx, filepath.Join(w.state, "journal"), other.Key())
				if err != nil {
					return nil, err
				}
			}
			var about aboutRecord
			if json.Unmarshal(metadata, &about) != nil {
				return nil, fmt.Errorf("cannot establish dependencies of %s", other.Key())
			}
			for _, dependency := range about.DependsOn {
				if dependency == string(key) {
					return nil, fmt.Errorf("%s still depends on %s", other.Key(), key)
				}
			}
		}
	}
	metadata := entry.About()
	if outcome == "applied" {
		if len(entry.Identity()) != 0 && string(entry.Identity()) != identity {
			return nil, fmt.Errorf("recovery cannot replace the prior identity")
		}
		if len(metadata) == 0 {
			metadata, err = journal.RecoveryAbout(ctx, filepath.Join(w.state, "journal"), key)
			if err != nil {
				return nil, err
			}
		}
		var about aboutRecord
		if json.Unmarshal(metadata, &about) != nil || about.Address == "" {
			return nil, fmt.Errorf("%s lacks recovery addressing metadata", key)
		}
	}
	// The attestation must be durable before changing the ledger; it is not a cloud effect.
	if err := journal.RecordDiagnostic(ctx, h.journal, key, "", journal.Diagnostic{Event: "recovery_assertion", Stage: "recovery", Outcome: outcome, Message: fmt.Sprintf("operator verified outcome at ledger version %s; identity %s", version, identity)}); err != nil {
		return nil, err
	}
	if err := h.check(ctx); err != nil {
		return nil, err
	}
	if outcome == "absent" {
		err = h.log.Forget(ctx, key, current)
	} else {
		err = h.log.Resolve(ctx, key, []byte(identity), metadata, current)
	}
	if err != nil {
		return nil, err
	}
	if err := journal.RecordDiagnostic(ctx, h.journal, key, "", journal.Diagnostic{Event: "success", Stage: "recovery", Outcome: outcome}); err != nil {
		return nil, errors.Join(fmt.Errorf("ledger recovered but recording completion failed"), err)
	}
	return []Row{{"key": string(key), "identity": identity, "status": "recovered_" + outcome}}, nil
}

type recoveryStatement struct{ resource, outcome, identity, version string }

func (recoveryStatement) statement() {}

// Recover records an explicitly verified outcome for one pending resource, without cloud calls.
// The caller must establish that the original operation has finished before asserting absence.
func Recover(resource, outcome, identity, version string) Statement {
	return recoveryStatement{resource: resource, outcome: outcome, identity: identity, version: version}
}

type recoveryPlan struct {
	config   ledgerWAL
	decision recoveryStatement
	args     Args
}

func (p recoveryPlan) Open(ctx context.Context) (Rows, error) {
	d := p.decision
	rows, err := p.config.Recover(ctx, d.resource, d.outcome, d.identity, d.version, p.args)
	if err != nil {
		return nil, err
	}
	return &sliceRows{rows: rows, i: -1}, nil
}
