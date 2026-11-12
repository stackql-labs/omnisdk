package omnisdk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// Read inspects this collection without leases, journals, cloud calls or filesystem writes.
// Entries are individually consistent; this is not a collection-wide transactional snapshot.
// Pending identity is the previous committed identity, not proof of the pending effect.
// Intent bodies and provider parameters are deliberately omitted.
func (w ledgerWAL) Read(ctx context.Context) ([]Row, error) {
	if w.collection == "" || strings.ContainsAny(w.collection, "/\\\x00") || w.collection == "." || w.collection == ".." || w.state == "" {
		return nil, fmt.Errorf("ledger read requires a collection name and state directory")
	}
	authorized, err := facade.Authorize(ctx, facade.Permission{Resource: "collection/" + w.collection, Action: "read"})
	if err != nil {
		return nil, err
	}
	ctx = authorized
	root := filepath.Join(w.state, "ledger")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return []Row{}, nil
	} else if err != nil {
		return nil, err
	}
	log, err := ledger.OpenFile(root)
	if err != nil {
		return nil, err
	}
	entries, err := log.List(ctx, w.collection+"/")
	if err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(entries))
	for _, entry := range entries {
		if string(entry.Key()) == w.collection+"/.lease" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current, version, found, err := log.Get(ctx, entry.Key())
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		var about aboutRecord
		if len(current.About()) > 0 {
			if err := json.Unmarshal(current.About(), &about); err != nil {
				return nil, fmt.Errorf("ledger metadata %s: %w", current.Key(), err)
			}
		}
		phase := "pending"
		if current.Phase() == facade.LedgerLive {
			phase = "live"
		}
		out = append(out, Row{"collection": w.collection, "key": string(current.Key()), "resource_key": strings.TrimPrefix(string(current.Key()), w.collection+"/"), "identity": string(current.Identity()), "phase": phase, "version": string(version), "provider": about.Provider, "address": about.Address, "addressed_by": about.AddressedBy, "depends_on": about.DependsOn})
	}
	return out, nil
}

// ReadLedger returns read-only resource metadata for one collection in stateDir.
// It does not create missing state or return intent bodies or provider parameters.
func ReadLedger(ctx context.Context, collection, stateDir string) ([]Row, error) {
	return (ledgerWAL{collection: collection, state: stateDir}).Read(ctx)
}
