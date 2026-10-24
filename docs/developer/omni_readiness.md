# Omni compatibility readiness

Historical CI reference: **658/733 passed, 75 failed** in
[run 39960274810](https://github.com/stackql/stackql-devel/actions/runs/39960274810),
using the downloaded `stackql_linux_omni_robot_reports` artifact.

The latest completed local full run is **661/733 passed, 72 failed**
(`/private/tmp/omni-remainder7-full-robot/output.xml`), up from 651/733. Comparing individual
scenarios found ten new passes and no lost passes. This run includes asynchronous mutation
completion, host-owned external SQL relations, declared transport metadata and the host's
legacy HTTP 404 read policy. The latter resolves the S3 sibling-read cancellation and positive
INSERT SELECT failure while preserving the original 404 diagnostic.

All eight async/await scenarios and the Kubernetes JSON-path scenario pass. All four external
PostgreSQL scenarios ran with `SHOULD_RUN_DOCKER_EXTERNAL_TESTS:true` and passed; they were
actually executed, rather than counted as passes through the usual local gate.

Local MCP failures and build-version metadata make the local total different from CI; compare
individual scenarios as well as totals. A join also varies in row order between runs. No Robot
input, assertion, or provider file was changed. The earlier 639/734 aggregate involved edited
inputs and is withdrawn as parity evidence.

The migration order is behavioral parity first, removal of the `any-sdk` dependency second,
and semantic improvements afterward. Existing SQLite boolean representation (`0`/`1`) remains
part of the parity target for this work.

## View and inbox binding follow-up

The subsequent unchanged mocked-query/server-variable run passed **370/404**, versus
367/404 for the same scenarios in the last full run. Four previously failing view cases passed:
polymorphic S3 with explicit inputs, the EC2 view-parameter scenario, repeated EC2 VPC view
invocation, and EC2 instance region tuple replacement. One previously passing join returned the
same two rows in the opposite order. The identical ordering failure exists in earlier artifacts
(`/private/tmp/omni-parity-order-robot/output.xml` and `omni-parity7-robot/output.xml`);
this run is not evidence that ordering parity is resolved.

Document-view variants now recognize declared dialect/required-input predicates. Caller constant
and IN bindings replace literal view inputs while preserving column-to-column dependencies and
business predicates. The SDK resolver no longer wires an already-bound inbox slot because a
different required input is missing. Bindings remain identity transfers; request conversion stays
in input transformations. No RDBMS code enters the SDK.

Validation: `/private/tmp/omni-inbox-next-robot/output.xml`, comparison
`/private/tmp/omni-inbox-next-comparison.json`, full SDK race checks
`/private/tmp/omni-inbox-next-full-sdk.log`, host race checks
`/private/tmp/omni-inbox-next-host.log` and lint `/private/tmp/omni-inbox-next-lint.log` (zero issues).
Robot inputs, assertions and provider files remain unchanged. This 404-case subset does not replace
the completed 733-case full-run total above.

A focused confirmation passed **5/5**, including all four recovered view cases and the
intermittent join-order case (`/private/tmp/omni-inbox-next-confirm-robot/output.xml`). Its passing
rerun does not remove the observed ordering instability from the backlog.

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
| Stackql Mocked From Cmd Line | 329/398 | 361/398 |
| Stackql Oci Auth | 11/11 | 11/11 |
| Stackql Odata Pushdown | 24/24 | 24/24 |
| Stackql Postgres Housekeeping | 6/6 | 6/6 |
| Stackql Rest Pagination | 11/11 | 11/11 |
| Stackql Server Variables | 3/6 | 6/6 |
| Stackql Sessions | 34/35 | 35/35 |
| Stackql Sessions Postgres | 8/8 | 8/8 |
| Stackql Xml Walker | 15/15 | 15/15 |

## Remaining failures

The latest local run has 37 mocked-query failures, 31 MCP failures and four CLI/metadata failures.
The historical CI reference passed all 85 MCP tests; the local MCP count is not evidence that
this batch introduced 31 SDK regressions.

Remaining failures include parameterized/resource-view composition, analytics-cache namespace
resolution, multi-step dataflow, remaining expression/projection differences and provider-specific mutation completion,
transaction diagnostics, metadata fidelity, XML request/response shapes, non-HTTP local execution,
and deeper server/view binding composition. Information-schema objects on explicitly configured external SQL connections now use host-owned
relation handles. Correlated subqueries and recursive provider CTEs still require analyzer/executor work. This is not a complete compatibility cutover.

The rewritten S3 ABAC test document explicitly maps UPDATE to `put_bucket_abac`. An earlier diagnosis
incorrectly inspected the native source document instead. The missing `Bucket` input was fixed by retaining path-level server variables after mock URL rewrites;
both the transformed update and obsolete-version diagnostic now pass.

GraphQL (9/9), OCI (11/11), OData (24/24), REST pagination (11/11), XML walker (15/15), function
projection (8/8), legacy CLI (25/25), and dependencies (8/8) remain green. The remaining failures
are not being treated as intentional differences or a cutover-ready result.

## Validation and artifacts

- Historical CI: `/private/tmp/omni-ci-39960274810/output.xml` (658/733).
- Latest completed full local run: `/private/tmp/omni-remainder7-full-robot/output.xml` (661/733).
- Async/await: `/private/tmp/omni-remainder3-async-robot/output.xml` (8/8).
- External PostgreSQL, Docker gate enabled: `/private/tmp/omni-remainder5-external-robot/output.xml` (4/4).
- Kubernetes transport binding: `/private/tmp/omni-remainder6-k8s/output.xml` (1/1).
- Scenario comparison: `/private/tmp/omni-remainder7-comparison.json` (ten gains, zero losses against the prior full run).
- Response-exchange race checks: `/private/tmp/omni-remainder7-docx.log`; final vet: `/private/tmp/omni-remainder7-vet.log`.
- SDK checks: `/private/tmp/omni-remainder-final-sdk.log`.
- Host race checks and lint: `/private/tmp/omni-remainder-final-host.log`,
  `/private/tmp/omni-remainder-final-lint.log` (zero lint issues).

The local retry mock has an older Python IPv4 listener alongside Docker's IPv6 listener on port 1199.
The final retry test uses `RETRY_MOCK_BASE_URL=http://localhost:1199`, matching the SDK request's host,
so reset, request, and counter assertions hit the same mock. The existing Python process was left
running; retry counts and expected output were not changed.

Both checkouts contain required changes: the SDK, StackQL's intrinsic adapter, and its GraphQL
registry URL rewriter. Testing used a temporary Go modfile pointing at the SDK checkout;
CI must consume the updated SDK and adapter together to measure the new result. API notes remain relative to `v0.1.5-beta06` in [api_changes.md](api_changes.md).
