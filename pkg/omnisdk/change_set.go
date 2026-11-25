package omnisdk

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/stackql-labs/omnisdk/internal/ledger"
	"github.com/stackql-labs/omnisdk/internal/system_g/facade"
)

// ChangeSet is the portable JSON representation of an IaC preview. Supply the
// same execution scope and binding inputs when applying it.
type ChangeSet struct {
	Version int              `json:"version"`
	Target  ChangeSetTarget  `json:"target"`
	Desired json.RawMessage  `json:"desired"`
	Changes []ResourceChange `json:"changes"`
	Scope   string           `json:"scope"`
}

// ChangeSetTarget identifies the registry and ledger used to make a preview.
type ChangeSetTarget struct {
	Registry   string `json:"registry"`
	Collection string `json:"collection"`
	State      string `json:"state"`
}

// ResourceChange records the action selected for one desired-state resource.
type ResourceChange struct {
	Key          string   `json:"key"`
	Action       string   `json:"action"`
	Reason       string   `json:"reason"`
	DependsOn    []string `json:"depends_on,omitempty"`
	Precondition string   `json:"precondition,omitempty"`
}

// Preview reads the current ledger and provider state without dispatching
// mutations or creating ledger and journal files.
func Preview(ctx context.Context, registry string, desired Statement, wal WALConfig, args Args) (ChangeSet, error) {
	statement, ok := desired.(desiredStatement)
	if !ok {
		return ChangeSet{}, errors.New("omnisdk: preview requires desired state")
	}
	prepared, err := Prepare(registry, statement, wal, args)
	if err != nil {
		return ChangeSet{}, err
	}
	p := prepared.(*desiredPlan)
	ctx, err = authorizeExecution(ctx, args, facade.Permission{Resource: "collection/" + p.name, Action: "converge"})
	if err != nil {
		return ChangeSet{}, err
	}
	target, err := p.changeSetTarget()
	if err != nil {
		return ChangeSet{}, err
	}
	raw, err := marshalDesiredResources(statement.resources)
	if err != nil {
		return ChangeSet{}, err
	}
	scope, err := p.previewScope()
	if err != nil {
		return ChangeSet{}, err
	}
	log := ledger.ReadOnlyFile(filepath.Join(p.state, "ledger"))
	changes, err := p.preview(ctx, log)
	if err != nil {
		return ChangeSet{}, err
	}
	return ChangeSet{Version: 1, Target: target, Desired: raw, Changes: changes, Scope: scope}, nil
}

// ApplyChangeSet prepares an approved preview for execution. Open rechecks its
// decisions under the collection lease before any provider mutation.
func ApplyChangeSet(registry string, changeSet ChangeSet, wal WALConfig, args Args) (Plan, error) {
	if changeSet.Version != 1 {
		return nil, fmt.Errorf("omnisdk: unsupported change set version %d", changeSet.Version)
	}
	resources, err := ParseResources(changeSet.Desired)
	if err != nil {
		return nil, err
	}
	prepared, err := Prepare(registry, DesiredState(resources), wal, args)
	if err != nil {
		return nil, err
	}
	p := prepared.(*desiredPlan)
	target, err := p.changeSetTarget()
	if err != nil {
		return nil, err
	}
	scope, err := p.previewScope()
	if err != nil {
		return nil, err
	}
	if target != changeSet.Target || scope != changeSet.Scope {
		return nil, errors.New("omnisdk: IaC plan target or execution scope changed")
	}
	p.approved = make(map[string]ResourceChange, len(changeSet.Changes))
	for _, change := range changeSet.Changes {
		if _, exists := p.approved[change.Key]; exists {
			return nil, fmt.Errorf("omnisdk: duplicate change for %q", change.Key)
		}
		change.DependsOn = slices.Clone(change.DependsOn)
		p.approved[change.Key] = change
	}
	return p, nil
}

func (p *desiredPlan) changeSetTarget() (ChangeSetTarget, error) {
	registry, err := filepath.Abs(p.registry)
	if err != nil {
		return ChangeSetTarget{}, err
	}
	state, err := filepath.Abs(p.state)
	return ChangeSetTarget{Registry: registry, Collection: p.name, State: state}, err
}

func (p *desiredPlan) previewScope() (string, error) {
	resources, err := marshalDesiredResources(p.resources)
	if err != nil {
		return "", err
	}
	type providerScope struct {
		Provider, Tenancy string
		Params            map[string]string
	}
	aliases := make(map[string]providerScope, len(p.args.ProviderAliases))
	for name, binding := range p.args.ProviderAliases {
		aliases[name] = providerScope{binding.Provider, binding.Tenancy, binding.Params}
	}
	params := p.args.Params
	if len(params) == 0 {
		params = nil
	}
	return previewDigest([]any{resources, params, p.args.Endpoint, aliases, p.args.Principal, p.args.IACRollback})
}

func marshalDesiredResources(resources []ManagedResource) (json.RawMessage, error) {
	specs := make([]map[string]any, 0, len(resources))
	for _, r := range resources {
		typ, body := r.Via()
		inbound := make([]map[string]string, 0, len(r.Inbound()))
		for _, a := range r.Inbound() {
			inbound = append(inbound, map[string]string{"from": a.From, "as": a.As})
		}
		specs = append(specs, map[string]any{
			"key": r.Key(), "provider": r.Provider(), "address": r.Address(), "desired": json.RawMessage(r.Desired()),
			"write_once": resourceParams(r), "inbound": inbound, "via_type": typ, "via": body,
			"identity": r.Identity(), "addressed_by": r.AddressedBy(),
		})
		if id := lookupIdentity(r); id != "" {
			specs[len(specs)-1]["lookup_identity"] = id
		}
	}
	return json.Marshal(specs)
}

func previewDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func (p *desiredPlan) preview(ctx context.Context, log facade.Ledger) ([]ResourceChange, error) {
	resources := make(map[string]ManagedResource, len(p.resources))
	for _, resource := range p.resources {
		resources[resource.Key()] = resource
	}
	changes := make(map[string]ResourceChange, len(p.resources))
	identities := map[string]string{}
	visiting := map[string]bool{}
	var inspect func(string) error
	inspect = func(key string) error {
		if visiting[key] {
			return fmt.Errorf("omnisdk: IaC dependency cycle at %q", key)
		}
		if _, done := changes[key]; done {
			return nil
		}
		visiting[key] = true
		defer delete(visiting, key)
		r := resources[key]
		change := ResourceChange{Key: key}
		for _, a := range r.Inbound() {
			change.DependsOn = append(change.DependsOn, a.From)
		}
		slices.Sort(change.DependsOn)
		change.DependsOn = slices.Compact(change.DependsOn)
		for _, dep := range change.DependsOn {
			if resources[dep] == nil {
				continue
			}
			if err := inspect(dep); err != nil {
				return err
			}
			switch changes[dep].Action {
			case "create", "unknown":
				change.Action, change.Reason = "unknown", "Depends on the result of "+dep
			}
		}
		var err error
		if change.Action == "unknown" {
			_, change.Precondition, err = previewLedgerEntry(ctx, log, facade.LedgerKey(p.name+"/"+key))
		} else {
			change, identities[key], err = p.previewResource(ctx, log, r, change, identities)
		}
		if err != nil {
			return err
		}
		changes[key] = change
		return nil
	}
	out := make([]ResourceChange, 0, len(p.resources))
	for _, r := range p.resources {
		if err := inspect(r.Key()); err != nil {
			return nil, err
		}
		out = append(out, changes[r.Key()])
	}
	return out, nil
}

func (p *desiredPlan) previewResource(ctx context.Context, log facade.Ledger, r ManagedResource, change ResourceChange, identities map[string]string) (ResourceChange, string, error) {
	key := func(k string) facade.LedgerKey { return facade.LedgerKey(p.name + "/" + k) }
	entry, _, err := previewLedgerEntry(ctx, log, key(r.Key()))
	if err != nil {
		return change, "", err
	}
	one := *p
	one.resources = []ManagedResource{r}
	prior := func(string) map[string]any {
		if entry == nil {
			return nil
		}
		m, _ := decodeObject(entry.Plan())
		return m
	}
	outside := func(dep string) (string, error) {
		if id, ok := identities[dep]; ok {
			return id, nil
		}
		entry, _, found, err := log.Get(ctx, key(dep))
		if err != nil {
			return "", err
		}
		if !found || entry.Phase() != facade.LedgerLive {
			return "", fmt.Errorf("omnisdk: a resource reads from %s, which is not live", key(dep))
		}
		return string(entry.Identity()), nil
	}
	g, nodes, _, err := one.lower(prior, outside)
	if err != nil {
		return change, "", err
	}
	var identity string
	seen := false
	run := &managedRun{name: p.name, ledger: log, nodes: nodes, preview: true, targets: p.targets,
		previewDependencies: map[string][]string{r.Key(): p.externalDependencies(change.DependsOn)}}
	run.observeDiff = func(decision desiredDecision) error {
		if seen {
			return fmt.Errorf("omnisdk: multiple live rows for %s", key(r.Key()))
		}
		seen = true
		identity = decision.identity
		change.Action, change.Precondition = decision.action, decision.digest
		switch change.Action {
		case "create":
			if entry != nil && p.targets[r.Key()].createVerb == "insert" {
				return fmt.Errorf("omnisdk: %s is absent from the provider but still recorded in the ledger; reconcile it before planning a create", key(r.Key()))
			}
			change.Reason = "Resource absent"
		case "no-op":
			change.Reason = "Live state matches desired state"
		default:
			change.Reason = "Live state differs from desired state"
		}
		return nil
	}
	args := p.args
	args.managed = run
	args.ReadCache = nil
	pl, err := NewGraphQuery(p.registry, g, args)
	if err != nil {
		return change, "", err
	}
	rows, err := pl.Open(ctx)
	if err != nil {
		return change, "", err
	}
	defer rows.Close()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		return change, "", err
	}
	if !seen {
		return change, "", fmt.Errorf("omnisdk: no decision for %s", key(r.Key()))
	}
	return change, identity, ctx.Err()
}

// In-run dependencies have their own decisions and can advance their ledger
// versions during apply. Only external dependency versions belong to this check.
func (p *desiredPlan) externalDependencies(dependencies []string) []string {
	var external []string
	for _, dep := range dependencies {
		if p.targets[dep] == nil {
			external = append(external, dep)
		}
	}
	return external
}

func previewLedgerEntry(ctx context.Context, log facade.Ledger, key facade.LedgerKey) (facade.LedgerEntry, string, error) {
	entry, version, found, err := log.Get(ctx, key)
	if err != nil {
		return nil, "", err
	}
	if found && entry.Phase() == facade.LedgerPending {
		return nil, "", fmt.Errorf("omnisdk: %s has unresolved intent; recover it before planning", key)
	}
	return entry, string(version), nil
}

type desiredDecision struct {
	key, action, identity, digest string
}

func (o diffOperator) observe(ctx context.Context, row map[string]any) error {
	attr := func(name string) string { return hidden(o.node.Alias(), name) }
	identity, _ := row[attr("identity")].(string)
	action := "no-op"
	switch row[attr("status")] {
	case DiffAbsent:
		action = "create"
	case DiffDrift:
		action = "update"
		if o.run.targets[o.node.key].updateVerb == "replace" {
			action = "replace"
		}
	}
	normalized := make(map[string]any, len(row))
	for name, value := range row {
		normalized[name[len(attr("")):]] = value
	}
	actual := make(map[string]any, len(o.fields))
	for _, field := range o.fields {
		actual[field] = o.bound[hidden(o.node.live, field)]
	}
	_, version, err := previewLedgerEntry(ctx, o.run.ledger, o.run.keyOf(o.node.key))
	if err != nil {
		return err
	}
	dependencies := make(map[string]string)
	for _, dep := range o.run.previewDependencies[o.node.key] {
		_, depVersion, err := previewLedgerEntry(ctx, o.run.ledger, o.run.keyOf(dep))
		if err != nil {
			return err
		}
		dependencies[dep] = depVersion
	}
	digest, err := previewDigest([]any{version, normalized, actual, dependencies})
	if err != nil {
		return err
	}
	return o.run.observeDiff(desiredDecision{key: o.node.key, action: action, identity: identity, digest: digest})
}

func (p *desiredPlan) verifyPreview(ctx context.Context, log facade.Ledger) error {
	changes, err := p.preview(ctx, log)
	if err != nil {
		return err
	}
	if len(changes) != len(p.approved) {
		return errors.New("omnisdk: IaC plan resource set changed")
	}
	for _, current := range changes {
		approved := p.approved[current.Key]
		if approved.Action != current.Action || approved.Precondition != current.Precondition ||
			!slices.Equal(approved.DependsOn, current.DependsOn) {
			return stalePreview(current.Key)
		}
	}
	return nil
}

func stalePreview(key string) error {
	return fmt.Errorf("omnisdk: IaC plan for %q is stale; generate a new preview", key)
}
