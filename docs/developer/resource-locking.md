# Resource locking and ledger recovery

Managed graph runs atomically acquire their logical resource footprint in a shared ledger. Two
writers cannot hold exclusive access to the same key. Shared dependency guards can overlap;
disjoint writes and independent consumers can proceed. Mutations of one key within
a run are also serialized, including sibling nodes and fanout invocations. This applies to managed
IaC paths, not arbitrary unmanaged SDK queries or external cloud clients. All writers of an object
must use the same collection/resource key and the same state store; aliases in separate collections
are not recognised as the same physical object.

The concrete rules are:

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
Independent members within a stage still execute concurrently. Ordinary queries have no checkpoint
wrappers. Ownership, identity and dependency records remain in the ledger after release. A later
writer must acquire the key again.

The AWS example therefore releases completed VPC/security-group operations and the unused subnet
association before VM provisioning finishes. Inputs still consumed by the VM mutation remain
locked through its readiness poll. A subsequent graph observation wired from the VM retains its
lock until that observation completes. The example's separate HTTP `verify` command is a read and
does not extend the completed apply run's write lock.

Branch/global-filter/projection graphs conservatively retain their footprints to the run boundary.
Unselected candidate nodes or collapsed admission stages may likewise retain locks until the run
ends. Desired-state lowering and destruction currently retain collection-wide exclusivity because
they scan existing state. These are conservative fallbacks, not early-release guarantees.

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

An unresolved entry blocks collection destruction and graph compensation conservatively. Otherwise
a child that exists remotely but has no recorded identity could be omitted from the dependency DAG
and its parents deleted. Compensation first reclaims exclusive access and compares the exact
committed ledger version; it does not undo a resource changed by another run. Compensating deletes
are themselves journalled managed mutations.

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

Go tests cover concurrent attachments sharing a subnet, release before VM observation completes,
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
