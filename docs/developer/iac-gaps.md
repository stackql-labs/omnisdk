# IaC gaps

`ConvergeGraph` runs IaC as a graph — recall, read, diff, branch, then any gated mutation — with
the ledger, collection hold and compensation below. Its own gaps are in section 6.

What converging desired state does not do, what happens instead, and what closing each gap takes. The run model
these build on — intent before effect, compensation, correlation stamps — is in
[invariants.md](invariants.md) and the package docs of `internal/apply`, `internal/lease` and
`internal/unwind`.

## 1. Drift converges only where an update operation accepts the change

**Now.** Desired state is lowered to a graph ([desired.go](../../pkg/omnisdk/desired.go)) with one
update arm per `update` operation the resource's fields could reach. The diff chooses the arm: the
operation the analyzer ([analyze.go](../../internal/analyze/analyze.go)) finds accepts the object's
identity and every field being sent. An operation with a body is sent the whole desired document;
one without is sent only the changed fields. Where no operation qualifies, the diff fails with
`<key> has drifted and no update path converges it: …` and the run unwinds.

**Needs.**
- Per-field declarations that override the document. The rule takes any `update` operation that
  accepts a field, so EC2 `CidrBlock` drift routes to `AssociateVpcCidrBlock`, which adds a CIDR block
  rather than changing it. A declaration marking a field immutable, or naming the operation for it, is
  what stops that.
- A replace path for immutable fields: a delete and a create, in a declared order, on one key. Until
  then an immutable field with no declaration is either refused or misrouted as above.
- Removing a field through an operation without a body is refused: such an operation cannot express
  it.

## 2. Destroy works from what the ledger records

**Now.** `Destroy()` under `NewLedger` deletes every live key of the collection
([destroy.go](../../pkg/omnisdk/destroy.go)), dependents first, each delete journalled before it is
sent and the key forgotten after. What each key is — its address, how it is addressed, what it
depends on — is recorded in the ledger when desired state resolves it. A refused delete is retried
while other deletes make progress; what still refuses is reported outstanding with the provider's
reason.

**Needs.**
- The same record for a hand-written graph's managed nodes: today only desired state writes it, so a
  graph's keys are reported outstanding by a destroy.
- Protections that refuse a delete for a reason other than a dependent — see
  [known_issues.md](known_issues.md).

## 3. A create is awaited only where its document says so

**Now.** A method declaring `x-stackQL-beta` is followed by the call it names, polled until that
call reports done ([beta.go](../../internal/system_g/exchange/docx/beta.go)); a dependent runs only
then. Done with an error fails; never done, or the followed call itself failing, is reported as
unknown. The base Google documents declare none, so a caller supplies it as a document patch.

**Needs.**
- The declaration in the registry's own documents, rather than in every caller's patch.
- A test of the IaC path: the effector compiles through the same `Spec`, so it awaits too, but only
  the query path is exercised.

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

- **Two undo paths.** Desired state is lowered to a graph and undone through the journal, which
  restores an update's prior intent. A hand-written graph's failed run deletes what it created,
  addressed by `AddressedBy`; an update or delete it made stays done, and is reported as completed.
- **One object per managed key.** A mutation node that runs for several rows writes every row to the
  same ledger key.
- **No correlation stamp.** Nothing links a created object back to its key, so losing the ledger
  orphans it.
- **No CLI.** It is reachable from Go only.

**Needs.**
- Journal-driven undo for a hand-written graph's managed nodes, as desired state has.
- A key per row for fan-out, derived from the node's inputs.
- The correlation parameter on `Managed`, stamped at create and used to adopt.
- A `doc-graph`-style CLI command taking `managed`.

## 7. Smaller limits

- **Tags given as create parameters are not converged.** The read returns them, but
  `TagSpecification.*` params are not desired state, so a tag edited elsewhere is not compared or
  put back.
- **Local disk only.** The ledger relies on `O_EXCL` and `link`, which are unreliable on NFSv3.
