# IaC gaps

`ConvergeGraph` runs IaC as a graph — recall, read, diff, branch, then any gated mutation — with
the ledger, collection hold and compensation below. Its own gaps are in section 6.

What `Converge` does not do, what happens instead, and what closing each gap takes. The run model
these build on — intent before effect, compensation, correlation stamps — is in
[invariants.md](invariants.md) and the package docs of `internal/apply`, `internal/lease` and
`internal/unwind`.

## 1. Drift on a live resource is refused

**Now.** A live key whose target no longer matches its intent asks the semantics for an update
exchange. `Converge` declares none for any resource (`providerWiring` in
[iac.go](../../pkg/omnisdk/iac.go)), so the step fails with
`apply: <key> has drifted and <exchange> declares no update path`
([apply.go](../../internal/apply/apply.go)), and the run unwinds.

**Needs.**
- A resource that states its update: the document's `update` method, or the create itself where the
  create is an upsert (`semantics.Declaration.Update`). Which one is a property of the API and is
  declared, never guessed — re-issuing a create that mints duplicates an object.
- A mutation that sends only what changed. The three-way merge already computes it; an API that takes
  a full replacement rather than a patch needs the whole desired document instead.
- Immutable fields — an EC2 CIDR — stay refused. Replacing an object is a delete and a create, and
  belongs to a replace path, not to update.

## 2. Nothing destroys a collection

**Now.** There is no entry point that removes what a collection created. Compensation removes what a
*failed* run did; a successful run's objects stay until removed by hand.

**Needs.**
- A facade entry point taking the collection name and state directory, both required.
- Teardown in reverse dependency order, each delete written to the ledger before it is sent, and
  repeated until a pass makes no progress — the shape `unwind` already has, driven from the ledger's
  live keys rather than a run's journal.
- A delete the provider refuses for a reason other than a dependent object (deletion protection,
  object lock) is reported as outstanding, never retried forever and never forced.

## 3. GCP creates are not awaited

**Now.** A GCP create returns a long-running Operation. The effector takes the call's acceptance as
success ([docrun.go](../../internal/effect/docrun/docrun.go) never polls), so a dependent key can
start before its source exists, and a create that later fails is recorded as live.

**Needs.**
- The effector polls the Operation to completion, using the poll a graph already supports
  (`docx.WithPoll`, `NewPollOverride`): status path, done value, interval and a bounded number of
  attempts, all required.
- A finished Operation carrying an error fails the step, so the run unwinds what it did.
- An Operation that never finishes within its bound is reported as unknown, not as failed: the object
  may exist, and the next run's read decides.

## 4. The lock covers the whole collection and is never renewed

**Now.** A run locks its entire collection (`lease.All()`) for a fixed five minutes, and nothing
renews it. Two runs touching disjoint resources in one collection exclude each other, and a run that
outlasts five minutes can have its lock taken by another while it is still sending.

**Needs.**
- Renewal while the run is alive, at a fraction of the expiry, and a run that cannot renew stops
  starting new keys.
- A fencing check: a run whose lock was taken must not write the ledger afterwards.
- Narrowing to the keys a run touches. The lock already takes a key set and conflicts by
  intersection; what is missing is computing each run's footprint before it starts.

## 5. Dependencies carry only an identity

**Now.** `Converge` takes a flat list of resources. A dependency (`Arrival{From, As}`) delivers the
source key's recorded identity and nothing else, reshaped by an optional `T_in`. A query graph's edge
can carry any column, fan out, and poll; an IaC dependency cannot.

**Needs.**
- A facade entry point that accepts a `Graph` of mutation nodes and lowers it onto `Converge`, rather
  than a second engine.
- A ledger entry that records the attributes an edge reads, not only identity — still resolved
  values, never a stored API response.
- A place on a mutation node for what a document cannot say: `identity`, `addressed_by` and
  `correlation_param`.
- A decision on fan-out: one node producing N objects needs a key per object, derived from its
  inputs, so a re-run addresses the same N.

## 6. ConvergeGraph

**Now.** `ConvergeGraph` runs any gated mutation durably, but:

- **`Converge` is not expressed through it.** The flat-list entry point still runs its own
  create-or-refuse step; section 1 applies to it, not to `ConvergeGraph`.
- **Compensation reverses creates only.** A failed run deletes what it created, addressed by
  `AddressedBy`; an update or delete it made stays done, and is reported as completed.
- **One object per managed key.** A mutation node that runs for several rows writes every row to the
  same ledger key.
- **No correlation stamp.** Nothing links a created object back to its key, so losing the ledger
  orphans it.
- **No CLI.** It is reachable from Go only.

**Needs.**
- `Converge(resources)` lowered to the recall, read, diff, branch graph.
- Update reversal from the journalled prior intent, through the inverse model.
- A key per row for fan-out, derived from the node's inputs.
- The correlation parameter on `Managed`, stamped at create and used to adopt.
- A `doc-graph`-style CLI command taking `managed`.

## 7. Smaller limits

- **Tags are stamped at create, not converged.** The read does not extract the tag set, so a tag
  edited elsewhere is not corrected.
- **Local disk only.** The ledger relies on `O_EXCL` and `link`, which are unreliable on NFSv3.
