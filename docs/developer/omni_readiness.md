# Omni compatibility readiness

Latest measured CI: **456/728 passed, 272 failed** in
[run 39714700734](https://github.com/stackql/stackql-devel/actions/runs/39714700734/job/119637217455).

After the compatibility fixes, **175 cases that failed in that CI job pass locally**.
The verified local aggregate is **600/730 passed, 130 failed**: a full 730-case run
plus a 506-case rerun after the final text/JSON NULL presentation correction. This is not a new CI result.
The local suite contains two new subquery regressions; both pass.

Compared with the previous completed local run (**587/728**), this branch recovers **11 existing
failures**, adds two passing regressions, and introduces **no failures among previously passing cases**.
There are 33 local failures among cases that passed in CI: 31 MCP cases and two CLI version cases,
matching the observed local TLS/build-metadata differences. That leaves **97 of the original
272 CI failures unresolved**. Do not compare raw local and CI totals as identical environments.
No tests were skipped, marked as expected failures, or given weaker assertions.

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
This does not yet provide general stored-view/table resolution, correlated subqueries or CTE execution.

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
| Mocked From Cmd Line | 184/393 | 303/395 |
| Oci Auth | 0/11 | 11/11 |
| Odata Pushdown | 9/24 | 24/24 |
| Postgres Housekeeping | 6/6 | 6/6 |
| Rest Pagination | 10/11 | 11/11 |
| Server Variables | 3/6 | 3/6 |
| Sessions | 34/35 | 35/35 |
| Sessions Postgres | 8/8 | 8/8 |
| Xml Walker | 12/15 | 15/15 |

## Remaining failures

Remaining failures include stored SQL views and materialized/user tables in mixed queries,
resource-view expansion, multi-step dataflow, SQL expression output, async/await and mutation-result
behavior, metadata fidelity, XML shapes, and server-variable integration. Stored relation and
information-schema discovery, correlated subqueries and CTE planning still require analyzer work;
the new handle interface supplies the execution seam for that work.

GraphQL (9/9), OCI (11/11), OData (24/24), REST pagination (11/11), XML walker (15/15), function
projection (8/8), legacy CLI (25/25), and dependencies (8/8) remain green. The remaining failures
are not being treated as intentional differences or a cutover-ready result.

## Validation and artifacts

SDK: `go build ./...`, `go vet ./...`, and `go test -race ./...` pass. Handle regressions cover
planning without IO, dependent bindings, cursor ownership, partial rows plus original errors,
required inputs, canceled opens, and rejection of mutation targets. Adapter race tests cover nested
subqueries, inner aggregation/order/limit boundaries, wildcard schemas, native values, NULL, constants
and LEFT JOIN. Adapter/output lint reports zero issues.

- Previous full baseline: `/private/tmp/omni-final-readiness/output.xml` (587/728).
- Current full Robot run: `/private/tmp/omni-relations-final-readiness/output.xml` (598/730).
- Final 506-case rerun: `/private/tmp/omni-relations-formats-robot/output.xml` (414/506).
- Rebot-merged results: `/private/tmp/omni-relations-aggregate/output.xml` (600/730).
- SDK checks: `/private/tmp/omni-relations-scalars-checks.log`.
- Handle regressions: `/private/tmp/omni-relation-tests.log`.
- Adapter/output race tests: `/private/tmp/omni-derived-final-tests.log`.
- Adapter/output lint: `/private/tmp/omni-derived-lint.log`.

The local retry mock has an older Python IPv4 listener alongside Docker's IPv6 listener on port 1199.
The final retry test uses `RETRY_MOCK_BASE_URL=http://localhost:1199`, matching the SDK request's host,
so reset, request, and counter assertions hit the same mock. The existing Python process was left
running; retry counts and expected output were not changed.

Both checkouts contain required changes: the SDK, StackQL's intrinsic adapter, and its GraphQL
registry URL rewriter. Testing used a temporary Go modfile pointing at the SDK checkout;
the companion `go.mod` was not changed. CI must consume the updated SDK and adapter to measure the
new result. API notes remain relative to `v0.1.5-beta06` in [api_changes.md](api_changes.md).
