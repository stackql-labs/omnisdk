# Resource locking and ledger recovery

> **AUTOMATIC ROLLBACK IS OFF BY DEFAULT.** Failures preserve partial progress, ownership,
> identities, pending intent and the journal. Successful effects are not automatically undone.
>
> **ROLLBACK ENABLED: X-LOCK THE MUTATION FOOTPRINT BEFORE THE FIRST EFFECT AND RETAIN IT
> THROUGH SUCCESS OR COMPENSATION.** Do not release locks and try to reacquire them after failure:
> a new dependent could otherwise attach to a created or mutated resource before undo.
>
> **ROLLBACK DISABLED: RELEASE OR DOWNGRADE AT LAST-USE CHECKPOINTS FOR GREATER CONCURRENCY.**
>
> The mutation footprint is the set of resources this execution may change, **not the whole named
> ledger collection**. Unrelated resources in that collection remain available in both modes.

Both modes acquire the declared footprint atomically before execution: **conservative two-phase
locking (2PL)**. Rollback-enabled execution retains its exclusive target locks and shared dependency
guards through the entire forward and compensation phases. Rollback-disabled graph execution can
shrink that footprint as operations and their consumers finish; it never adds locks after shrinking.

Exclusive target protection blocks both competing writes and new dependents using that target.
A shared dependency guard protects an upstream object from mutation/deletion without blocking other
consumers. There is no unlock/relock gap on the path into compensation. Lost protection or an
unresolved outcome leaves rollback outstanding; opting in does not make an unknowable effect safe
to reverse.

The execution/session inputs are explicit and stateless:

- `Args.TxnRollback` (`"txn_rollback": true`) enables the retained-lock policy for ordinary scoped
  query graphs and SQL. It can also enable it on managed IaC execution.
- `Args.IACRollback` (`"iac_rollback": true`) is the IaC-specific opt-in for `ConvergeGraph` and
  desired-state runs. It does not enable rollback for ordinary SQL.
- Both default to false. Neither uses process-global state, environment-variable lookup, or sticky
  session defaults inside the SDK.

Ordinary SQL uses the same execution machinery. Supply `SQLStatement(query, tables, managed...)`
with `NewLedger(collection, state, runID)` and `Args{TxnRollback: true}`. Managed declarations name
mutation aliases, stable resource keys and response identities; the scope and footprint must be
known before effects. An ordinary mutating graph can likewise use `GraphStatement` with managed
targets. A rollback-enabled mutation without this contract is refused before dispatch. Arbitrary
multi-object DML still needs its affected resource set and undo metadata resolved explicitly;
rollback is not inferred from raw SQL text or arbitrary provider identifiers. Unclassified legacy
catalog methods do not supply that contract and reject `TxnRollback` rather than silently ignore it.
Read-only SQL has no effects to compensate and does not acquire exclusive targets merely because
the rollback flag is set.

Explicit `Destroy` remains an explicit action, independent of automatic rollback policy. Failure
rows report `rollback: "disabled"`, `"completed"`, or `"outstanding"`; the original forward failure
remains a failure even if compensation succeeds. Graph compensation currently reverses known
creates and reports updates/deletes it cannot reverse. Desired-state compensation uses declared
inverse and prior-intent restore paths. Unavailable or unsafe compensation remains outstanding.

StackQL will ultimately need **`SET TXN_ROLLBACK=true`** and **`SET IAC_ROLLBACK=true`**, mapped into
these per-execution inputs. **The SQL `SET` statements are not implemented here.** The locking policy
applies to compensatable ordinary mutations as well as IaC; it does not make external cloud APIs
ACID transactions.

Managed graph runs atomically acquire their logical resource footprint in a shared ledger. Two
writers cannot hold exclusive access to the same key. Shared dependency guards can overlap;
disjoint writes and independent consumers can proceed. Mutations of one key within
a run are also serialized, including sibling nodes and fanout invocations. This applies to scoped managed
queries and IaC, not uncoordinated SDK calls or external cloud clients. All writers of an object
must use the same collection/resource key and the same state store; aliases in separate collections
are not recognised as the same physical object.

The more aggressive **rollback-disabled** rules are:

- A mutation owns its `Managed` resource key exclusively until that operation and its declared
  readiness condition finish. Authors must assign the key of the object actually being mutated.
- A node consuming another resource through a beta edge or recall holds a shared use guard on it.
  Other consumers may share that guard; a writer or deleter cannot.
- A stage releases its inputs after its last consumer completes. An observation after a managed
  mutation retains a shared guard on that mutation's resource, not all its ancestors.

For `subnet -> attach VM -> observe VM ready`, two different VM attachments can share the subnet
guard. The subnet becomes writable after both attachment stages finish, while the separate VM
observations retain their VM guards. Existing unrelated attachments are untouched. If attachment
confirmation and VM readiness are bundled in one document beta/poll operation, the guard lasts
through that whole operation; declare separate nodes to obtain separate release checkpoints.
Current rules come from managed ownership and declared graph references, not a new lock-expression
DSL or inference from arbitrary API parameter names.

Stage checkpoints exhaust all rows before releasing anything or passing rows downstream.
Independent members within a stage still execute concurrently. Unmanaged ordinary queries have no checkpoint
wrappers. Ownership, identity and dependency records remain in the ledger after release. A later
writer must acquire the key again.

The AWS example uses declarative desired-state specs and therefore currently retains its bounded
footprint through each apply. Its separate stopped-state apply retains the instance and dependency
guards through the stop waiter. Go graph tests separately verify early release after the last
consumer. HTTP/SSH verification happens after the apply and does not extend its write lock.

Branch/global-filter/projection graphs conservatively retain their footprints to the run boundary.
Unselected candidate nodes or collapsed admission stages may likewise retain locks until the run
ends. Desired-state execution currently retains its bounded resource footprint for the whole run
because it lowers reconciliation branches and external bindings from state. Full collection
`Destroy` still uses a collection-wide lease for its deletion scan. These are conservative
fallbacks, not early-release guarantees.

## Durable transitions

| Checkpoint | Durable evidence | Recovery rule |
| --- | --- | --- |
| Before a mutation | Pending ledger intent and write-ahead journal | A write-ahead failure prevents dispatch. |
| Await handle obtained | Journal method and addressing inputs, before polling | Handle survives restart; ownership remains pending until readiness completes. |
| Mutation/readiness succeeds | Resolved identity, intent and deletion/dependency metadata | Publish rows only after commit; release after the last consumer. |
| Response lost, identity missing, or commit fails | Pending intent; any recorded handle remains | Block subsequent mutations; do not replay or guess success. |
| Definite provider rejection | Prior resolved state restored, or a rejected new entry removed | No remote change is claimed. |
| Delete dispatched | Pending intent preserving the prior identity and metadata | A lost response cannot become a blind repeated delete. |
| Delete succeeds | Ledger tombstone | Release the run's lease after cleanup. |

Transport retry suppression for mutations survives nested document retry policies. Polling still
performs its declared observation loop; that is separate from resending a mutation. The pending
phase deliberately does not claim whether dispatch actually happened: a crash immediately before
and immediately after dispatch can leave the same durable evidence.

An unresolved protected entry blocks automatic compensation. An unresolved entry anywhere in a
collection blocks the current full-destruction path conservatively. Otherwise a child that exists
remotely but has no recorded identity could be omitted from the delete DAG and its parents removed.
Compensation uses the locks retained from before forward execution and compares committed ledger
versions and current dependents before deleting a target. Compensating graph deletes are themselves
journalled managed mutations.

## Lease lifetime and limits

Claims carry unique acquisition IDs. A scope stores a single CAS-updated claim registry, allowing
atomic multi-key acquisition and release without lock ordering. Empty registries remain as internal
bookkeeping. Legacy single-claim records are read without discarding an active lease; older binaries
must not run concurrently with this format.

Active runs renew five-minute leases every third of the lease interval and check ownership before
dispatch and commit. Renewal failure cancels the run; expired handles cannot renew or release a
successor's claim. Cleanup uses an independent context so caller cancellation does not leak a lease.
A crashed process leaves its claim until expiry, while unresolved ledger intent survives expiry
and still blocks mutations.

Cloud APIs generally do not enforce our fencing tokens. A suspended process or request already
accepted remotely cannot be revoked by a local lease; strict remote fencing is not claimed.
Automatic pending-outcome reconciliation and automatic execution of saved polling handles remain
separate work. Current safety is to preserve uncertainty and refuse blind writes. File-ledger
backing remains local disk, not NFS or a distributed lock service.

## Verification

Go tests cover default-off rollback for graphs and desired state, explicit opt-in compensation,
ordinary SQL transaction scope, blocked adornments before and during undo, unrelated concurrent
writers in the same ledger collection, concurrent attachments sharing a subnet, release before VM observation completes,
disjoint/overlapping claims, atomic narrowing, persistent claims across reopen,
expired holders, lease renewal/loss, serialized same-key invocations, last-consumer release,
multi-row stage checkpoints, intent/journal/commit failures, missing identities, and uncertain
create/delete outcomes across restart. The normal SDK convergence and destruction tests remain
regression coverage.

The [AWS Robot example](../../cicd/examples/aws-nginx/README.md) inspects the real file ledger at
create, readiness, stop and teardown checkpoints. Additional cases drop accepted create/delete
responses and kill the runner during readiness. They assert retained recovery evidence and no repeated or
competing mutations. These are mock/API-boundary tests, not proof of all filesystem power-loss
behaviour or real-cloud service semantics.

Execution diagnostics are automatic under `<state>/journal/<run-id>.diagnostics.jsonl`.
They record dispatch and terminal outcomes independently of forward compensation steps;
request/response dumps are not required. Missing terminal evidence after a crash is not proof that
an effect failed. Pending ledger entries still require outcome recovery before another mutation.
