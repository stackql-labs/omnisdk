# Repository invariants

Rules that hold across the codebase. They are not style preferences: each one exists because
breaking it has already cost something. Where a rule is machine-checkable it is checked, and the
test is named.

## API shape

**1. Interfaces, not public structs.** Exported concrete structs are not the API. Keep concrete
types unexported and return interfaces from constructors. The one exception is a **serde DTO**: a
value that exists to be marshalled or handed across the boundary as data, with no behaviour —
`Args`, `Auth`, `Param`, `Tuning`. If it has methods, or callers might want a second implementation,
it is not a DTO.

*Not* "use a struct now and add an interface once there are two implementations". Interface first.

**2. A feature is designed behind an interface before it is built.** Non-negotiable. A new
capability starts as a contract in `facade`, then an implementation.

**3. Scope is explicit input, never inferred.** Which account, which region, which collection, which
state directory — anything that decides *what a run acts on* is required from the caller. No
defaults, no environment fallbacks, no derivation from another flag. A wrong guess here applies real
changes to the wrong resources.

Auth credentials are not scope and may come from the environment.

## Layering

**4. `internal/system_g/facade` is the contract root and imports the standard library only.**
Enforced: `TestFacadeDependsOnStdlibOnly`.

**5. Durable-machinery packages see contracts only.** `ledger`, `merge`, `journal`, `lease` and
`semantics` depend on `facade` and nothing else in the module: the executor calls them, never the
reverse. Enforced: `TestLeafImplementationsSeeContractsOnly`.

**6. `pkg/omnisdk` is the public facade.** It owns its own DTOs; `system_g` stays internal.
`cmd/omnicli` imports nothing from `internal/` — it consumes the facade exactly as an external
caller would, which is what keeps the facade honest.

**RDBMS access belongs to the host.** Database discovery, connections, SQL execution and
database drivers must stay outside the library. Hosts pass relation handles through `Relation`
and return cursors through `Rows`; Omni does not receive a database connection. Database drivers
used solely by parity tests are test oracles, not production dependencies.
Enforced: `TestLibraryHasNoRDBMSDependencies`.

## Durability and effects

**7. Intent is recorded before the effect, never after.** Both the per-key ledger entry and the run
journal must receive durable acknowledgement before a mutating wire call. A crash between recording
intent and dispatch can leave the outcome unknown; intent alone never proves an effect occurred
or authorizes compensation. Recovery must establish the outcome before proceeding.

**8. Never guess about a destructive action.** If the target cannot be read, "did this land?" is
unknown and must be reported, not assumed. If a step updated an object rather than creating it, its
compensation restores the prior intent — it does not delete. Both of these were shipped wrong once
and destroyed, or would have destroyed, real resources.

**9. Persist resolved intent, never API responses.** Actual state is read live. Only identity is
kept from a response, and identity means everything the inverse call needs to address the object.

**10. Infrastructure content is explicit user intent.** Labels are ordinary user-declared request data,
never special injected metadata. The ledger and journal are authoritative; cloud tags never
recover intent, establish ownership or authorize adoption. Never stamp undeclared tracking data.

## Journal and Ledger

There are two persistent storage collections: journal and ledger.  These act in collaboration to support decalrative IAC and rolllback for bith SQL tansactions and also IAC.  This follows a SAGA pattern.

## Confidentiality

**11. `.claude/local/` is confidential.** Never commit it, publish it, or send its contents to any
external service.

## Concurrency

**12. Concurrency is built in from day one**, not retrofitted. In-process latches are ordered and
never held across a wire call; cross-run exclusion is a durable, expiring lease.


# Measurable quantities

1. A chain of keyed `left outer join` operators can be evaluated one partition of its
   leftmost exchange at a time. Let $R_i$ be exchange $i$'s response data, $P_{1,k}$ be
   partition $k$ of $R_1$, and $J$ be the left-join chain with its original predicates.
   Let $R_i(P_{1,k})$ contain the complete matching right-hand data needed at exchange $i$
   for that partition, including matches reached through preceding joins, with
   $R_1(P_{1,k}) = P_{1,k}$. For the same underlying data:

   $$
   R_1 = \biguplus_k P_{1,k}
   \quad\implies\quad
   J(R_1,\ldots,R_n)
   = \biguplus_k J(P_{1,k},R_2(P_{1,k}),\ldots,R_n(P_{1,k}))
   $$

   Here $\biguplus$ means `UNION ALL`: partitions preserve every row occurrence exactly once.
   The response-data working set for partition $k$, measured in bytes, is:

   $$
   W_k = \sum_{i=1}^{n}\operatorname{size}(R_i(P_{1,k}))
   $$

   With one partition active at a time, the largest such working set is $\max_k W_k$.
   Results can be emitted eagerly, and partition data discarded once no remaining work
   references it. This bounds retained response data; execution buffers and materialized
   join results must be accounted for separately. Global operations such as sorting or
   aggregation require retaining data across partitions.

2. After a successful IaC run, the ledger alone must suffice for subsequent operations.
   Journals since the last successful run may be consulted to recover interrupted or failed work.

3. Runs sharing a coordination store and lock scope must acquire compatible, durably
   acknowledged lease claims atomically before dispatching protected mutations. Exclusive
   claims exclude conflicting access; shared guards permit compatible operations. Claims
   expire unless renewed; loss of a claim must stop further protected dispatch. An already
   dispatched provider request may still complete. Independent coordination stores provide
   no mutual exclusion. Durability is a backend contract: file sync, database commit, or
   an equivalent durable remote acknowledgement, not a requirement to use `fsync`.

4. **Ledger:** retain authoritative intent, identities, dependencies and unresolved state.
   GC may remove obsolete records only when unreferenced, preserving version fencing.

5. **Journal:** retain effect ordering, compensation data and observation checkpoints.
   GC requires durable completion, ledger sufficiency and no execution/recovery references;
   claiming deletion must atomically prevent new references. Age alone is insufficient.

6. **Diagnostics/logs:** explanatory only; deletion must not affect execution or recovery.
   GC follows configured age/size limits after rotation closes active writers.

   These GC conditions are requirements, not claims of implemented collection.

7. **Access control:** when enabled, authorization must precede protected reads, mutations
   and control actions. Policy-required result masking must occur before results leave Omni.

8. **Concurrent access:** sessions must not leak credentials, private data or permissions
   across principals, regardless of concurrency. Test with at least ten concurrent sessions
   across three principals, including permitted and denied operations.

9. **Rollback:** opt-in saga compensation must support declared compensatable mutations
   in both IaC and ordinary queries. Compensation failure must remain explicit and recoverable.

10. **Provider SQL transactions:** reuse IaC journal, ledger, leases and compensation;
    do not require desired state. Bind handles to authenticated sessions; authorize every
    operation. Resume requires fresh authentication, ownership checks and new capabilities.
    Retain write locks through commit/undo; growing scope must never release earlier locks.
    Flush deferred dependencies before reads/results. Persist the exact declared inverse
    before accepting completion. Reject unsupported inverses before dispatch; recover
    uncertain outcomes explicitly with version fencing, never by replaying requests.

    Current migration limits and SQL usage: [transactions](transactions.md).

## Verification of measurable quantities

Tests cover specific guarantees, not blanket proof of each point.

| Point | Evidence / remaining gap |
| --- | --- |
| 1 | `TestKeyedLeftJoinPartitionsPreserveDuplicatesAndUnmatchedRows` checks partition equivalence. `TestResolvedQueryStreamsInBoundedMemory` verifies ~100 MiB of distinct output under a 35 MiB sampled post-GC heap-growth limit, with a full-retention negative control. It requires at least 8 MiB of further output with no net live-heap growth in each half, plus shrinking heap while output advances: evidence of streaming and reclamation without full-output retention for this workload. Full join-memory scaling remains unproved. |
| 2 | `TestCompletedLedgerSurvivesJournalAndDiagnosticsRemoval`; Robot `Completed Run History Is Not Required For Teardown`. |
| 3 | Lease contention, shared guards, expiry and renewal-loss tests in `internal/lease` and `resource_locks_test.go`. |
| 4 | Ledger conformance tests and `TestFileRecreateAfterForget` check retained state and stale-version rejection. Compaction is not implemented. |
| 5 | `TestManagedLedgerCrashBoundaries` injects intent/journal/commit failures; `TestAwaitCheckpointsPersistWithoutBecomingCompensationSteps` checks recovery evidence. GC eligibility/reference coordination is not implemented. |
| 6 | `TestDeletingDiagnosticsPreservesRecoveryCheckpoints` and point 2 check deletion independence. Rotation/retention GC is not implemented. |
| 7 | Common-gate access tests check denials before effects; `TestResultRedactionPrecedesPublicRows` checks the redaction hook. Automatic policy-to-mask binding is not established. |
| 8 | `TestConcurrentSessionsKeepPrincipalPermissionsAndRowsIsolated`: 12 overlapping sessions, three principals; run under `-race`. |
| 9 | `TestRollbackPreventsAdornmentThroughUndo` checks both rollback options; `TestSQLTxnRollbackLocksOnlyMutationScope` checks SQL integration; `TestFailedCompensationRemainsVisibleAndCanBeRecovered` checks failed undo and later cleanup. |

| 10 | `transaction_test.go` covers retained locks, durable queues, explicit recovery, fresh authorization, concurrent ownership and stale handles. stackql Robot covers eager/lazy execution, read/RETURNING barriers, crash resume and recovery. The streaming memory test also exercises transaction reads. See migration limits above. |
