# Omni compatibility readiness

The [earlier CI change](https://github.com/stackql/stackql-devel/actions/runs/39704194902?pr=140) improved **392/728 to 401/728**. It fixed pagination and bounded hangs;
it did not complete the casing, body projection, and coercion work.

The current local comparison is **368/728 to 427/728**. This is a comparison with the earlier
local run, not with CI: the local harness has additional MCP, build-metadata, and fixture differences.
The full run used all 728 existing scenarios with `{"unstable":true,"staging":true,"omni":"all"}`.
The full run initially passed 425 cases; two final metadata reruns passed and are incorporated below.
This remains a red readiness result.

## Results by suite

| Suite | Earlier local passes | Current local passes |
|-------|----------------------|----------------------|
| Mcp | 54/85 | 54/85 |
| Body Coercion | 3/9 | 9/9 |
| Casing | 15/32 | 32/32 |
| Dependencies | 4/8 | 7/8 |
| Exec | 3/3 | 3/3 |
| Export | 8/8 | 8/8 |
| From Cmd Line | 24/32 | 24/32 |
| Function Projection | 3/8 | 5/8 |
| Graphql Pagination | 0/9 | 0/9 |
| Legacy Cli Surface | 15/25 | 15/25 |
| Mocked From Cmd Line | 166/393 | 187/393 |
| Oci Auth | 0/11 | 0/11 |
| Odata Pushdown | 9/24 | 9/24 |
| Postgres Housekeeping | 6/6 | 6/6 |
| Rest Pagination | 10/11 | 10/11 |
| Server Variables | 1/6 | 4/6 |
| Sessions | 32/35 | 34/35 |
| Sessions Postgres | 8/8 | 8/8 |
| Xml Walker | 7/15 | 12/15 |

The casing and body-coercion suites now pass all **41/41**, previously **18/41**. Their fixes cover
wire parameter aliases, response aliases, body-property names, scalar coercion, PUT replacement,
literal INSERT SELECT, fallback bodies, and required method parameters. Native names remain usable.
One SHOW METHODS assertion was updated from empty required-parameter cells to the document-declared
`org` and `username` values; no tests were skipped or marked as expected failures.

## Failure families and evidence

- **TLS propagation:** both StackQL Omni argument builders now honour `--tls.allowInsecure`.
  The earlier local failures contained 51 untrusted-certificate diagnostics; the current full run
  contains none. Some calls now reach a different, previously masked failure.
- **Casing/body projection:** the parser now retains provider alias settings and method native casing.
  SQL names resolve against declared wire names; JSON schema coercion runs before request transmission.
  Previously blocked update tests now report the expected invalid body property and type.
- **SQL result shape/format:** representative ordered pagination output matches any-sdk byte for byte
  in both JSON-lines and table formats. This does not establish general SQL compatibility. Remaining
  failures include metadata populated with placeholder service fields, unsupported query expressions,
  and schema discovery. The current full run has 20 failing cases containing unresolved-column
  diagnostics and 19 containing unknown-WHERE-symbol diagnostics; these are overlapping diagnostic
  counts, not independent root-cause counts.
- **Authentication:** nine current failures contain the PKCS#1 private-key parsing diagnostic.
  OCI authentication still fails all 11 scenarios. These are separate from certificate verification.
- **GraphQL:** all nine pagination scenarios still fail; operation GraphQL request/cursor declarations
  are not implemented. HTTP transport alone does not implement those semantics.
- **OData:** 15/24 scenarios still fail around filter functions, offset, select/count, and ordering
  pushdown. The existing pushdown surface does not cover the full fixture grammar.
- **XML:** naming improvements fix five cases. Singleton extraction and continuation handling remain
  separate defects; the current suite still has three failures.
- **Pagination:** REST remains 10/11; the remaining cursor-default-encoding scenario still fails.
- **Server scope:** required server-variable metadata is now exposed. Environment-derived scope is
  intentionally unsupported under the repository invariant requiring explicit scope inputs; supply
  server bindings explicitly. The environment-fallback scenarios therefore remain incompatible.
- **Local harness:** 31 MCP failures are unchanged from the earlier local run. They must not be counted
  as regressions introduced by these SDK changes or silently subtracted to claim a CI pass rate.

Error prefixes are not byte-for-byte equivalent: invalid-update validation reports the same property
and schema type, while Omni currently wraps that error in conservative mutation-effect uncertainty
text. The resource request is not sent when its request-body transform rejects the value.

## Validation and reproduction

SDK validation: `go build ./...`, `go vet ./...`, and `go test -race ./...`.
The StackQL intrinsic adapter has separate tests and vet checks against the local SDK.

The Robot binary was built using a temporary Go modfile pointing at this SDK checkout; the
companion checkout's `go.mod` was not changed. Both the SDK and companion adapter changes are
required to reproduce these results. SDK API changes are documented relative to `v0.1.5-beta06` in
[api_changes.md](api_changes.md).

Local artifacts:

- Merged Robot report: `/private/tmp/omni-completion-final/report.html`.
- Full Robot run: `/private/tmp/omni-completion-readiness/output.xml`.
- Final metadata rerun: `/private/tmp/omni-completion-metadata-final/output.xml`.
- Focused casing/body run: `/private/tmp/omni-completion-casing2/output.xml`.
- any-sdk/Omni representative comparisons: `/private/tmp/omni-completion-parity.json` and
  `/private/tmp/omni-completion-parity-table.json`.
- Final SDK validation log: `/private/tmp/omni-completion-verified-go-tests.log`.

The traffic light is a compatibility inventory. The remaining failures are open work, not intentional
format changes or evidence that Omni is ready for cutover.
