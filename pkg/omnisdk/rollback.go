package omnisdk

import (
	"context"
	"fmt"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/lease"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Rollback never expands a shrinking claim. Compensatable execution retains
// its original write footprint throughout forward work and undo.
func (a Args) rollbackEnabled() bool { return a.IACRollback || a.TxnRollback }

func desiredFootprint(name string, resources []ManagedResource) facade.KeySet {
	var writes, reads []facade.LedgerKey
	for _, resource := range resources {
		writes = append(writes, facade.LedgerKey(name+"/"+resource.Key()))
		for _, input := range resource.Inbound() {
			reads = append(reads, facade.LedgerKey(name+"/"+input.From))
		}
	}
	return lease.Using(writes, reads)
}

func beginRollback(ctx context.Context, forward *held, name, _ string) (*held, error) {
	if err := forward.check(ctx); err != nil {
		return nil, fmt.Errorf("rollback cannot trust its retained locks: %w", err)
	}
	entries, err := forward.log.List(ctx, name+"/")
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !forward.lease.Keys().Contains(entry.Key()) || strings.HasPrefix(strings.TrimPrefix(string(entry.Key()), name+"/"), ".") {
			continue
		}
		if entry.Phase() == facade.LedgerPending {
			return nil, fmt.Errorf("%s has unresolved intent; recover its outcome before rollback", entry.Key())
		}
	}
	return forward, nil
}

func appliedKeys(run *managedRun) []facade.LedgerKey {
	run.mu.Lock()
	defer run.mu.Unlock()
	var keys []facade.LedgerKey
	for _, effect := range run.applied {
		keys = append(keys, effect.key)
	}
	return keys
}
