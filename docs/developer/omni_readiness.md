# Omni compatibility readiness

Latest measured CI: **456/728 passed, 272 failed** in
[run 39714700734](https://github.com/stackql/stackql-devel/actions/runs/39714700734/job/119637217455).

After the compatibility fixes, **203 cases that failed in that CI job pass locally**.
The verified fresh full local run is **631/733 passed, 102 failed**. This is not a new CI result.

Compared with the previous verified local aggregate (**625/732**), this batch recovers **five
existing failures**, adds a passing CASE-join regression, and retains **every previously passing case**.
There are 33 local failures among cases that passed in CI: 31 MCP cases and two CLI version cases,
including local TLS/client-output and build-metadata differences. That leaves **69 of the original
272 CI failures unresolved**. Do not compare raw local and CI totals as identical environments.
No tests were skipped, marked as expected failures, or given weaker assertions.

The migration order is behavioral parity first, removal of the `any-sdk` dependency second,
and semantic improvements afterward. Existing SQLite boolean representation (`0`/`1`) remains
part of the parity target for this work.

## Current compatibility batch

The mocked suite now passes **334/398**, compared with **328/397** before this batch.
Five existing failures recover, the new CASE-join regression passes, and no previously passing
mocked case regresses. The recovered cases are both Route53 XML record creations, the explicit
GitHub page-limit case, and both transformed JSON/XML CASE joins. A previously intermittent
view/TVF case is not counted as recovered.

This batch adds lazy conditional evaluation, SQLite arithmetic, projection evaluation after
filtering, declared XML request envelopes and validated fragments, per-provider page budgets,
and placement of body-bound read controls without losing body defaults. RDBMS work remains in
the host adapter, behind relation interfaces.

Full evidence: `/private/tmp/omni-batch-full-robot/output.xml`; mocked evidence:
`/private/tmp/omni-batch2-robot/output.xml`. The full run confirms the same five recoveries and
zero regressions. SDK build, vet, and full race tests pass; adapter race tests and lint pass.

## Implemented

- Provider `resources` references and resource-only documents, including `serviceDoc` and external
  operation pointers into `services-split` documents. The loader retains declared directories;
  it no longer forces every reference into `services/<basename>`.
- Operation-local schema and path-parameter resolution, immutable views of cached external
  documents, file-metadata cache invalidation, and rejection of references outside the bundle.
- Legacy SELECT method matching (`select`, `list`, `aggregatedList`, `get`), INSERT matching
  (`insert`, `create`), and DELETE matching (`delete`) when no explicit verb mapping exists.
  Explicit empty mappings remain disabled. No destructive methods are inferred from HTTP verbs.
- Case-insensitive HTTP verb segments in operation pointers.
- Qualified server-variable bindings, including variables retained after a private/mock URL rewrite.
  Path/server scope bindings no longer discard rows by comparing short request identifiers against
  same-named response URIs. Required server inputs remain enforced by the exchange, separately from
  operation-signature selection.
- Google service-account RSA private keys in both PKCS#8 and PKCS#1 PEM encodings.
- StackQL adapter support for configured `srcPrefix` in both metadata and query execution.

- OCI RSA signing, raw/profile credential validation, and adapter credential propagation.
- GraphQL cursor/keyset/offset/page-info traversal with deadlines, error handling and row selection.
- JSON property/wildcard/index/slice selection and selected row schemas; bounded `allOf` materialisation.
- OData projection/filter/order/skip/count controls, preserving original SQL hints across staging
  and acknowledging consumed offsets to prevent applying them twice.
- XML singleton extraction beside metadata, transformed continuation tokens, and declared scalar types.
- Provider service metadata and SQL-backend filtering for SHOW; canonical/native resource IDs.
- Document/resource/method request translation and transposition inheritance, header defaults,
  structured AWS query inputs, and JSON body input discovery.
- Constant function values in mutations and request bindings; path scope echoed when absent from rows.
- XML selection through declared array-item wire names, including `allOf` annotations.
- Declared signing service identity survives private/mock endpoint rewrites; method `serviceName`
  overrides document `info.x-serviceName`.
- Nested body input translation and schema coercion before `x-stackQL-stringOnly` serialization.
- Scalar comparison calls with SQL NULL semantics; host translation of scalar comparisons and `||`.
- Per-provider host credentials forwarded for every source, including sources after derived handles.
- Declared wildcard expansion at staging boundaries, joined `COUNT(*)` without ambiguous column
  expansion, and host translation to Omni's existing lateral table-function interface.

## Relation composition in this branch

`Relation`/`NewRelation` and `Args.Relations` let a front end supply opaque read handles with declared
inputs and columns. They participate in normal Omni resolution, bindings, projections, filters and
joins without a provider bundle. Openers run only during execution, receive declared inputs rather
than private join keys, own their storage/query execution, and transfer cursor ownership to Omni.
Mutations against these read handles are rejected.

The StackQL adapter now resolves uncorrelated FROM subqueries through those handles, including
nesting, constants, declared wildcard schemas, and separate SQL boundaries for aggregation,
DISTINCT, window functions and ORDER BY/LIMIT. Child SQL results retain native numbers and NULL.
Text writers render NULL consistently with backend results; JSON retains an actual JSON null.
The missing `toInt`/`toBool` template helpers also unblock transformed XML reads and joins.

Some previously green derived-table scenarios fell through to the legacy planner. Moving them into
Omni increases actual Omni coverage without increasing the traffic-light pass count for those cases.
The host adapter also resolves stored views, materialized views and user tables through its SQL-system
metadata interfaces. Storage reads become opaque handles; Omni receives no connection, SQL executor
or database-discovery code. An architecture test rejects production database dependencies.

Nonrecursive CTEs use the same boundaries, with lexical scoping, earlier-definition references,
explicit column names and nested shadowing. Stored view definitions keep an independent scope.
Recursive provider CTEs and correlated subqueries still require additional analyzer/executor work.

## Results by suite

| Suite | Latest CI passes | Verified local passes |
|-------|------------------|-----------------------|
| Mcp | 85/85 | 54/85 |
| Body Coercion | 9/9 | 9/9 |
| Casing | 32/32 | 32/32 |
| Dependencies | 7/8 | 8/8 |
| Exec | 3/3 | 3/3 |
| Export | 8/8 | 8/8 |
| From Cmd Line | 26/32 | 28/32 |
| Function Projection | 5/8 | 8/8 |
| Graphql Pagination | 0/9 | 9/9 |
| Legacy Cli Surface | 15/25 | 25/25 |
| Mocked From Cmd Line | 184/393 | 328/397 |
| Oci Auth | 0/11 | 11/11 |
| Odata Pushdown | 9/24 | 24/24 |
| Postgres Housekeeping | 6/6 | 6/6 |
| Rest Pagination | 10/11 | 11/11 |
| Server Variables | 3/6 | 3/6 |
| Sessions | 34/35 | 35/35 |
| Sessions Postgres | 8/8 | 8/8 |
| Xml Walker | 12/15 | 15/15 |

## Remaining failures

Remaining failures include parameterized/resource-view composition, analytics-cache namespace
resolution, multi-step dataflow, CASE/arithmetic expressions, async/await and mutation-result behavior,
transaction diagnostics, metadata fidelity, XML request/response shapes, non-HTTP local execution,
and server-variable integration. Information-schema discovery, correlated subqueries and recursive
provider CTEs also require analyzer/executor work. This is not a complete compatibility cutover.

GraphQL (9/9), OCI (11/11), OData (24/24), REST pagination (11/11), XML walker (15/15), function
projection (8/8), legacy CLI (25/25), and dependencies (8/8) remain green. The remaining failures
are not being treated as intentional differences or a cutover-ready result.

## Validation and artifacts

SDK: `go build ./...`, `go vet ./...`, and `go test -race ./...` pass. Handle regressions cover
planning without IO, dependent bindings, cursor ownership, partial rows plus original errors,
required inputs, canceled opens, and rejection of mutation targets. Adapter race tests cover nested
subqueries, inner aggregation/order/limit boundaries, wildcard schemas, native values, NULL, constants
and LEFT JOIN. New adapter regressions cover stored tables/views, joined counts, table functions,
CTE scope and relational boundaries. Adapter lint reports zero issues.

- Previous full baseline: `/private/tmp/omni-final-readiness/output.xml` (587/728).
- Previous verified aggregate: `/private/tmp/omni-relations-aggregate/output.xml` (600/730).
- Previous full Robot run: `/private/tmp/omni-coverage3-robot/output.xml` (623/731).
- Previous mocked rerun: `/private/tmp/omni-coverage5-robot/output.xml` (328/397).
- Previous merged results: `/private/tmp/omni-coverage-aggregate/output.xml` (625/732).
- Current full Robot run: `/private/tmp/omni-batch-full-robot/output.xml` (631/733).
- Current comparison: `/private/tmp/omni-batch-comparison.json`.
- Remaining failure details: `/private/tmp/omni-coverage-final-failures.json`.
- SDK checks: `/private/tmp/omni-batch-final-sdk.log`.
- Adapter race tests: `/private/tmp/omni-batch-host-race.log`.
- Adapter lint: `/private/tmp/omni-batch-lint.log`.

The local retry mock has an older Python IPv4 listener alongside Docker's IPv6 listener on port 1199.
The final retry test uses `RETRY_MOCK_BASE_URL=http://localhost:1199`, matching the SDK request's host,
so reset, request, and counter assertions hit the same mock. The existing Python process was left
running; retry counts and expected output were not changed.

Both checkouts contain required changes: the SDK, StackQL's intrinsic adapter, and its GraphQL
registry URL rewriter. Testing used a temporary Go modfile pointing at the SDK checkout;
the companion `go.mod` was not changed. CI must consume the updated SDK and adapter to measure the
new result. API notes remain relative to `v0.1.5-beta06` in [api_changes.md](api_changes.md).
