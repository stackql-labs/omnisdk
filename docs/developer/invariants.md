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

**Ledger.** The durable, versioned state of each managed resource key in a collection. It holds the
last resolved intent, provider identity and resource metadata. Beginning a mutation adds a pending
proposal while preserving that resolved state. Resolution commits the proposal and identity, or
removes the key from live state after a confirmed delete. Writes check the version read by the caller.

**Journal (run WAL).** The durable, append-only sequence of mutation intents for one run. Each
mutation record identifies the resource key, exchange and operation form, and preserves prior intent
where applicable. Recovery uses journal evidence to investigate uncertain outcomes. Compensation
uses the sequence in reverse for effects known to have landed. Await checkpoints preserve the method
and inputs needed to resume observation.

Managed mutations in IaC and provider SQL transactions require both the pending ledger entry and
the journal record to be durable before dispatch. Either record can survive a crash before dispatch,
so recovery must establish the effect's outcome before treating the mutation as applied.

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

10. **Provider SQL transactions:** all stackql SQL transactions reuse IaC journal, ledger, leases and compensation;
    do not require desired state. Bind handles to host sessions. When auth is enabled,
    authorize every operation and verify ownership on resume; otherwise user ownership
    is not guaranteed. Resume always issues new capabilities.
    Retain write locks through commit/undo; growing scope must never release earlier locks.
    Flush deferred dependencies before reads/results. Persist the exact declared inverse
    before accepting completion. Reject unsupported inverses before dispatch; recover
    uncertain outcomes explicitly with version fencing, never by replaying requests.

    Current migration limits and SQL usage: [transactions](transactions.md).

## Verification of measurable quantities

Tests cover specific guarantees, not blanket proof of each point.

| Point | Evidence / remaining gap |
| --- | --- |
| 1 | `TestKeyedLeftJoinPartitionsPreserveDuplicatesAndUnmatchedRows` checks partition equivalence. `TestResolvedQueryStreamsInBoundedMemory` verifies ~100 MiB of distinct output under a 35 MiB sampled post-GC heap-growth limit, with a full-retention negative control. It requires at least 8 MiB of further output with no net live-heap growth in each half, plus shrinking heap while output advances: evidence of streaming and reclamation without full-output retention for this workload. `TestJoinWorkingSetHeap` checks keyed left and inner joins, a listed equality join, and a cross join. It gates the driver to one active partition inside a single open cursor, varies output and working payload independently, and checks sampled live heap against twice the working payload plus 1 MiB for execution overhead. The ungated bind join can queue additional driving rows, which are outside this one-active-partition bound. |
| 2 | `TestCompletedLedgerSurvivesJournalAndDiagnosticsRemoval`; Robot `Completed Run History Is Not Required For Teardown`. |
| 3 | Lease contention, shared guards, expiry and renewal-loss tests in `internal/lease` and `resource_locks_test.go`. |
| 4 | Ledger conformance tests and `TestFileRecreateAfterForget` check retained state and stale-version rejection. Compaction is not implemented. |
| 5 | `TestManagedLedgerCrashBoundaries` injects intent/journal/commit failures; `TestAwaitCheckpointsPersistWithoutBecomingCompensationSteps` checks recovery evidence. GC eligibility/reference coordination is not implemented. |
| 6 | `TestDeletingDiagnosticsPreservesRecoveryCheckpoints` and point 2 check deletion independence. Rotation/retention GC is not implemented. |
| 7 | Common-gate access tests check denials before effects; `TestResultRedactionPrecedesPublicRows` checks the redaction hook. Automatic policy-to-mask binding is not established. |
| 8 | `TestConcurrentSessionsKeepPrincipalPermissionsAndRowsIsolated`: 12 overlapping sessions, three principals; run under `-race`. |
| 9 | `TestRollbackPreventsAdornmentThroughUndo` checks both rollback options; `TestSQLTxnRollbackLocksOnlyMutationScope` checks SQL integration; `TestFailedCompensationRemainsVisibleAndCanBeRecovered` checks failed undo and later cleanup. |

| 10 | `transaction_test.go` covers retained locks, durable queues, explicit recovery, fresh authorization, concurrent ownership and stale handles. stackql Robot covers eager/lazy execution, read/RETURNING barriers, crash resume and recovery. The streaming memory test also exercises transaction reads. See migration limits above. |


# Appendices

## Appendix I: Basic ideas review

Relational algebra includes projections $\pi$, joins $\bowtie$, and filters $\sigma$. Filters only reduce row counts; ordinary column projections do not increase them. Joins can multiply row counts, making them the main consideration here. Table JT-1 describes retained response data; operator state and materialized output are accounted for separately.

For keyed joins, $P_{a,k}$ is partition $k$ of the driving exchange $a$, and $R_i(P_{a,k})$ is the complete matching data needed from exchange $i$. For general execution, $C_{i,k}$ is exchange $i$'s response data retained during work unit $k$, whether in memory or on disk.

**Table JT-1**: Join types and response-data working sets.

**Enforced heap bound for the JT-1 regression**

$$
\max_{t\,\text{sampled}}\bigl(H_{\mathrm{post\text{-}GC}}(t)-H_{\mathrm{baseline}}\bigr)
\;\le\; 2\max_k W_k + 1\,\mathrm{MiB}.
$$

`TestJoinWorkingSetHeap` enforces this bound with one active partition per open query and measures $W_k$ in input payload bytes. A 272 KiB working set has a hard test limit of **1,568 KiB**.

| Join Type | Response-data working set | Comments |
|---|---|---|
| Left | $W_k = \sum_{i=1}^{n}\operatorname{size}(R_i(P_{1,k}))$ | Partition the leftmost preserved exchange, as in point 1. |
| Right | $W_k = \sum_{i=1}^{n}\operatorname{size}(R_i(P_{n,k}))$ | Partition the rightmost preserved exchange; equivalent to the corresponding left join with operands swapped. |
| Inner | $W_k = \sum_{i=1}^{n}\operatorname{size}(R_i(P_{a,k}))$ | Choose driving exchange $a$; returns matching pairs only. |
| Full Outer Join | $W_k = \sum_{i=1}^{n}\operatorname{size}(C_{i,k})$ | Unmatched rows require accounting; a sorted merge can stream them. |
| Cross Join (Cartesian product) | $W_k = \sum_{i=1}^{n}\operatorname{size}(C_{i,k})$ | Output row count is $\prod_{i=1}^{n}\lvert R_i\rvert$; retaining that output is a separate cost. |

`TestMultiJoinHeapShrinksBetweenPages` checks that sampled post-GC live heap drops by at least 512 KiB after each of three drained pages while the simulated next page waits, in both a keyed lookup chain and a fan-out chain.

**Table JT-2**: Binary join output cardinality, counting duplicate row occurrences.

Let $a=\lvert A\rvert$, $b=\lvert B\rvert$, $h$ be the number of matching pairs, and $u_A,u_B$ count unmatched input rows.

| Join type | $N_{\text{out}}$ | Tight upper bound |
|---|---|---|
| Inner | $h$ | $ab$ |
| Left | $h+u_A$ | $a\max(1,b)$ |
| Right | $h+u_B$ | $b\max(1,a)$ |
| Full outer | $h+u_A+u_B$ | $\max(ab,a+b)$ |
| Cross (Cartesian product) | $ab$ | $ab$ |

Apply these bounds at each join in a chain. An $n$-way Cartesian product emits $N_{\text{out}}=\prod_{i=1}^{n}\lvert R_i\rvert$ rows. Its nested iteration needs only $O(n)$ cursor state, in addition to retained inputs and a bounded output buffer.

These counts matter when a consumer retains the fragment's output. Its data size is $S_{\text{out}}=\sum_{r\in O}\operatorname{size}(r)$, counting every output row occurrence. General full sorting of unsorted output and explicit materialization require working storage proportional to that projected output size. A subquery alone does not require materialization.

Exact median/percentiles and collection aggregates can also require storage proportional to their contributing values. Some window frames require an entire partition. DISTINCT and grouped aggregation can have linear-size state in the worst case; bounded accumulators such as SUM and COUNT need only constant state per group.

For any join type, global or oversized pages add per-exchange response-data retention; they do not themselves multiply it. The response-data working-set bound is:

$$
W'_{\max} \le \max_k W_k + \sum_{i=1}^{n} B_i
$$

Here $B_i$ bounds extra bytes retained at exchange $i$ beyond the partition working set, with each retained input buffer stored once. Join direction and paging method determine those sizes.

Sorting and aggregation may require state across partitions. Working-set size includes retained data in memory or on disk; spilling does not reduce it. Some aggregates need only bounded accumulator state. Other extension functions are treated as projection adaptors for this analysis.


Looking at the general case, the uppper bound for one join in a larger query $Q$ composed of many joins, can be defined in terms of result set cardinality upper bounds $m_i$ and the maximum storage permitted per output row, termed $s_{max}$:


$$
S_{\mathrm{out}}(Q)\le s_{\max}\,U(Q),
\qquad U(R_i)=m_i
$$

$$
U(A\Join_t B)=
\begin{cases}
U(A)U(B) & t=\mathrm{inner,\ cross}\\
U(A)\max(1,U(B)) & t=\mathrm{left}\\
U(B)\max(1,U(A)) & t=\mathrm{right}\\
\max\!\bigl(U(A)U(B),\,U(A)+U(B)\bigr) & t=\mathrm{full\ outer}
\end{cases}
$$

That is an algebraic upper bound. Fixed predicates and constraints can make the actual maximum smaller.
