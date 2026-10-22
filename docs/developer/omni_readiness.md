# Omni compatibility readiness

Latest measured CI: **456/728 passed, 272 failed** in
[run 39714700734](https://github.com/stackql/stackql-devel/actions/runs/39714700734/job/119637217455).

After the implemented compatibility fixes, **162 cases that failed in that CI job pass locally**.
The verified local aggregate is **583/728 passed, 145 failed**: a full 728-case run
plus targeted suite reruns after subsequent fixes. This is not a new CI result.

There are 35 local failures among cases that passed in CI: 31 MCP cases and two CLI version
cases, matching the already-observed local TLS/build-metadata differences, plus two SHOW METHODS
formatting regressions under verification. Do not compare the raw
local and CI totals as though they ran in the same environment. No tests were skipped, marked as
expected failures, or given weaker assertions.

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
| Mocked From Cmd Line | 184/393 | 286/393 |
| Oci Auth | 0/11 | 11/11 |
| Odata Pushdown | 9/24 | 24/24 |
| Postgres Housekeeping | 6/6 | 6/6 |
| Rest Pagination | 10/11 | 11/11 |
| Server Variables | 3/6 | 3/6 |
| Sessions | 34/35 | 35/35 |
| Sessions Postgres | 8/8 | 8/8 |
| Xml Walker | 12/15 | 15/15 |

## Remaining failures

The document fixes remove all missing-service-document diagnostics seen in the latest local full
run. Legacy SELECT diagnostics fall from 26 cases in CI to 15 locally; the PKCS#1 parsing diagnostic
falls from nine cases to zero. These are diagnostic counts, not guaranteed independent pass gains:
several formerly blocked calls now expose different failures.

Remaining failures include resource/view SQL expansion, multi-step dataflow, SQL expression output,
mutation return/await behavior, metadata fidelity, XML shapes, and server-variable integration.
Document retry policies and REST partial-result error propagation are implemented. GraphQL (9/9), OCI (11/11), OData (24/24), XML walker
(15/15), function projection (8/8), legacy CLI (25/25), and dependencies (8/8) pass their full suites.
The remaining failures are not being treated as intentional differences or a cutover-ready result.

## Validation and artifacts

SDK: `go build ./...`, `go vet ./...`, and `go test -race ./...`. New regressions cover split-file
schemas/cache freshness, legacy priority/explicit disablement, bundle confinement, real JWT
signature verification, and HTTP scope binding with original and rewritten server URLs.
The StackQL intrinsic adapter also has its tests and vet checks run against the local SDK.

- Full Robot run: `/private/tmp/omni-schema-readiness/output.xml`.
- Latest mocked-suite run: `/private/tmp/omni-wire-media-robot/output.xml`.
- GraphQL/OCI/OData: `/private/tmp/omni-read-controls-robot/output.xml`.
- Metadata: `/private/tmp/omni-metadata-predicates-robot/output.xml`.
- Merged results: `/private/tmp/omni-current-aggregate/output.xml`.
- SDK checks: `/private/tmp/omni-final-checks.log` (final checks in progress).

The local retry mock has an older Python IPv4 listener alongside Docker's IPv6 listener on port 1199.
The final retry test uses `RETRY_MOCK_BASE_URL=http://localhost:1199`, matching the SDK request's host,
so reset, request, and counter assertions hit the same mock. The existing Python process was left
running; retry counts and expected output were not changed.

Both checkouts contain required changes: the SDK, StackQL's intrinsic adapter, and its GraphQL
registry URL rewriter. Testing used a temporary Go modfile pointing at the SDK checkout;
the companion `go.mod` was not changed. CI must consume the updated SDK and adapter to measure the
new result. API notes remain relative to `v0.1.5-beta06` in [api_changes.md](api_changes.md).
