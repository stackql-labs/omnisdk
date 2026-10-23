# Omni compatibility readiness

Current CI reference: **658/733 passed, 75 failed** in
[run 39960274810](https://github.com/stackql/stackql-devel/actions/runs/39960274810),
using the downloaded `stackql_linux_omni_robot_reports` artifact.

The latest completed local full run is **651/733 passed, 82 failed**
(`/private/tmp/omni-parity8-robot/output.xml`). The final 404-case query/server-variable rerun after the
explicit Azure authentication fix passed **355/404**, with **49 failures**
(`/private/tmp/omni-parity10-robot/output.xml`). Two S3 view cases that passed in the
preceding full run failed here: one changed rows, and one aggregation returned no rows.
The cause is unresolved; stable regression-free parity is not established. A focused
race-instrumented CLI rerun passed two of the three S3 cases and reported no data race.
The failed case recorded an upstream HTTP 404 in its stderr artifact. The positive INSERT
source case records HTTP 404 `Project Not Found`; it remains failing. Local MCP failures and build-version
metadata make the local total different from CI; compare individual scenarios as well as totals.
A join also varies in row order between runs. No Robot input, assertion, or provider file was
changed. The earlier 639/734 aggregate involved edited inputs and is withdrawn as parity evidence.

The migration order is behavioral parity first, removal of the `any-sdk` dependency second,
and semantic improvements afterward. Existing SQLite boolean representation (`0`/`1`) remains
part of the parity target for this work.

## Current compatibility batch

This batch recovers error-result headers for plain and staged reads, explicit upstream read-error
opt-ins, forward references in INNER JOIN conditions, scalar XML policy-list projections, empty
JSON-selection diagnostics, and an empty INSERT/SELECT RETURNING-star result. Existing Robot scenarios and new unit tests cover these paths, including preservation of partial
rows and original errors.

SDK interfaces keep ignored-read diagnostics in the host. `StrictResponseSelection` is opt-in;
missing declared keys report a diagnostic when enabled, while declared empty arrays remain valid.
Scalar XML row metadata gives discovery and execution the same named column. RDBMS work remains
entirely in the host adapter, behind relation interfaces. Outer-join conditions keep their boundary.

The current batch additionally separates declared and synthesized AWS negotiation headers,
retains explicit body bindings and canonical projection names, projects constants and NULL,
resolves declared server environment annotations in the host, and executes INSERT SELECT's
complete read source behind a handle before mutation dispatch. RETURNING star names only the
target's fields. DELETE AWAIT RETURNING fails before dispatch. Explicit Azure API keys and bearer
tokens override default OAuth acquisition without requiring the Azure CLI. Fourteen failing tests
in the current CI artifact encounter that credential-precedence bug; fixing their authentication
blocker does not establish that all fourteen otherwise pass.

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

| Suite | Current CI passes | Latest completed local full run |
|-------|-------------------|----------------------------------|
| Mcp | 85/85 | 54/85 |
| Stackql Body Coercion | 9/9 | 9/9 |
| Stackql Casing | 32/32 | 32/32 |
| Stackql Dependencies | 8/8 | 8/8 |
| Stackql Exec | 3/3 | 3/3 |
| Stackql Export | 8/8 | 8/8 |
| Stackql From Cmd Line | 30/32 | 28/32 |
| Stackql Function Projection | 8/8 | 8/8 |
| Stackql Graphql Pagination | 9/9 | 9/9 |
| Stackql Legacy Cli Surface | 25/25 | 25/25 |
| Stackql Mocked From Cmd Line | 329/398 | 351/398 |
| Stackql Oci Auth | 11/11 | 11/11 |
| Stackql Odata Pushdown | 24/24 | 24/24 |
| Stackql Postgres Housekeeping | 6/6 | 6/6 |
| Stackql Rest Pagination | 11/11 | 11/11 |
| Stackql Server Variables | 3/6 | 6/6 |
| Stackql Sessions | 34/35 | 35/35 |
| Stackql Sessions Postgres | 8/8 | 8/8 |
| Stackql Xml Walker | 15/15 | 15/15 |

## Remaining failures

Remaining failures include parameterized/resource-view composition, analytics-cache namespace
resolution, multi-step dataflow, remaining expression/projection differences, async/await and mutation-result behavior,
transaction diagnostics, metadata fidelity, XML request/response shapes, non-HTTP local execution,
and deeper server/view binding composition. Information-schema discovery, correlated subqueries and recursive
provider CTEs also require analyzer/executor work. This is not a complete compatibility cutover.

The rewritten S3 ABAC test document explicitly maps UPDATE to `put_bucket_abac`. An earlier diagnosis
incorrectly inspected the native source document instead. The missing `Bucket` input was fixed by retaining path-level server variables after mock URL rewrites;
both the transformed update and obsolete-version diagnostic now pass.

GraphQL (9/9), OCI (11/11), OData (24/24), REST pagination (11/11), XML walker (15/15), function
projection (8/8), legacy CLI (25/25), and dependencies (8/8) remain green. The remaining failures
are not being treated as intentional differences or a cutover-ready result.

## Validation and artifacts

- Current CI: `/private/tmp/omni-ci-39960274810/output.xml` (658/733).
- Latest completed local full run: `/private/tmp/omni-parity8-robot/output.xml` (651/733).
- Full SDK race checks before the final auth fix: `/private/tmp/omni-parity8-final-sdk.log`.
- Explicit Azure override regression tests: `/private/tmp/omni-parity9-auth.log`.
- Final query/server-variable run: `/private/tmp/omni-parity10-robot/output.xml` (355/404).
- Focused race-instrumented views: `/private/tmp/omni-parity-race-views/output.xml` (2/3; no data race reported).
- Per-test comparison: `/private/tmp/omni-parity-final-evidence.json`.
- Final SDK checks: `/private/tmp/omni-parity9-final-sdk.log` (passed).
- Final host race checks and lint: `/private/tmp/omni-parity10-host.log`,
  `/private/tmp/omni-parity10-lint.log`.

The local retry mock has an older Python IPv4 listener alongside Docker's IPv6 listener on port 1199.
The final retry test uses `RETRY_MOCK_BASE_URL=http://localhost:1199`, matching the SDK request's host,
so reset, request, and counter assertions hit the same mock. The existing Python process was left
running; retry counts and expected output were not changed.

Both checkouts contain required changes: the SDK, StackQL's intrinsic adapter, and its GraphQL
registry URL rewriter. Testing used a temporary Go modfile pointing at the SDK checkout;
the companion `go.mod` was not changed. CI must consume the updated SDK and adapter to measure the
new result. API notes remain relative to `v0.1.5-beta06` in [api_changes.md](api_changes.md).
