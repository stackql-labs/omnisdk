# API changes

Migrating from `v0.1.5-beta06` to the current tree.

## Compile-time breaks

Custom implementations of `pkg/docparse/aot.Pagination` must implement three additional methods:

| Method | Return to retain the previous token-pagination behaviour |
|--------|---------------------------------------------------------|
| `Algorithm() string` | `""` |
| `ResponseTerminator() (key, location string)` | `"", ""` |
| `RequestTokenEncoding() string` | `""` (normal query escaping) |

The supplied StackQL document parser implements these methods. Callers using that parser need no
source changes. Keyed `omnisdk.Args`, `omnisdk.Tuning`, and `omnisdk.DocMethod` literals remain compatible;
positional literals must account for added fields or switch to keyed fields.

## Additions

- **Explicit Azure authentication:** an explicitly configured API key or bearer credential
  overrides default OAuth token acquisition. Planning does not require a service principal or
  invoke the Azure CLI before applying that request credential. Default OAuth behavior is retained.

- **Mutation star projections:** unqualified `RETURNING *` expands target columns only;
  source relation fields remain internal to INSERT SELECT execution.
- **Literal projections:** SELECT outputs may contain constants and NULL alongside resource
  columns. They are evaluated after row filtering, preserving their native values.
- **Server environment metadata:** optional `aot.ServerEnvironmentBindings` and
  `omnisdk.ServerEnvironmentBindings` expose explicitly declared `x-stackQL-envVar` names.
  `DocMethod.ServerEnvironment` carries the same metadata for method discovery. The SDK never
  reads those environment variables or substitutes server defaults. A host may resolve the
  annotations and pass ordinary query bindings, preserving explicit SQL precedence.
- **AWS signed headers:** declared `Accept` headers remain signed. Automatically synthesized
  response negotiation headers remain on the wire but are excluded from signing. Re-signing
  excludes the previous `Authorization` value.

- **Declared server precedence:** operations use their own server declaration, otherwise the
  path item's, otherwise the document's. Scope variables from the selected declaration remain
  accepted after endpoint rewrites; their defaults are never substituted for explicit caller scope.
- **Request programs:** optional `aot.RequestTransform` exposes a declared request-body program.
  It runs before signing and takes precedence over generic XML envelope serialization. The
  `golang_template_text_v0.1.0` evaluator is now registered alongside v0.3.0; unknown versions fail
  before dispatch. Existing request implementations need no additional methods.

- **Explicit upstream read-error policy:** `Args.HTTPReadErrors` accepts an
  `HTTPReadErrorPolicy` interface (`IsIgnored(int) bool`). A matching HTTP error ends that
  document-backed resource read cleanly without a synthetic row or another retry. Prior pages remain available.
  This opt-in never suppresses mutation or authentication failures. A policy may also implement
  `HTTPReadErrorObserver.IgnoredStatus(int, string)` for host-owned diagnostics. Both interfaces
  stay outside serialized arguments; nil retains strict handling.
- **Response selection diagnostics:** `Args.StrictResponseSelection` optionally reports an absent
  declared JSON row key, including JSON-path selections. Default behavior remains an empty read;
  a declared empty array remains successful in either mode. StackQL opts in for CLI parity.
- **Scalar XML rows:** optional `aot.ScalarRowSchema` declares the column associated with a
  scalar XML selection. The supplied parser exposes the terminal element as a column (for example,
  `PolicyNames/member` becomes `member`), consistently across discovery and row acquisition.

- **Conditional expressions:** the function catalogue supports lazy searched and simple CASE
  (`case_when`, `case_value`) plus three-valued `and`, `or`, and `not`. SQLite `if` and `iif`
  skip unselected branches. Optional `sqlfn.LazyFunc` lets custom functions request deferred
  argument evaluation; existing `Func` implementations remain compatible. Scalar query outputs
  are evaluated after filtering, so rejected rows cannot trigger projection errors.
- **SQLite arithmetic:** scalar `+`, `-`, `*`, `/`, and `%` follow SQLite numeric conversion,
  integer division, overflow promotion, and NULL-on-division-by-zero behavior.
- **XML requests:** optional `aot.XMLRequest` exposes declared root names, declarations, and
  root attributes. The StackQL parser supplies these automatically. Structured XML fragments
  are validated; scalar strings remain escaped.
- **Read controls:** `Tuning.PageLimit` caps pages per resource read and ends cleanly at the cap.
  Global zero or negative means unlimited; provider zero inherits and negative removes the cap.
  Retries and polling are separate. Document-declared body controls remain in the JSON body
  and preserve existing defaults instead of becoming query parameters.

- **Template conversions:** document Go templates now provide `toInt` and `toBool`, including
  XML-to-JSON programs for transformed volumes. Their absent-value defaults are zero and false.

- **Analyzer-resolved read relations:** `omnisdk.Relation` combines table metadata with
  `Open(context.Context, map[string]any) (Rows, error)`. `NewRelation` builds an immutable
  declaration from explicit input and column metadata plus an opener. `Args.Relations` registers
  handles by exact address for `NewGraphQuery`, bypassing provider-document lookup for those
  addresses. Supply the same relations to `ResolveIn` as tables. They support ordinary graph
  bindings, projections, filters and joins, and reject mutation targets. Planning performs no
  calls to the opener. Each execution owns its cursor and closes it; openers must honor the
  supplied context and support independent concurrent executions. This is a read seam, not a
  SQL parser, database connector or recursive-CTE executor.

Opening a graph plan with an already-canceled context now returns the cancellation error immediately
without invoking read handles or provider exchanges.

- **Document request defaults:** declared header defaults and response `Accept` media types reach
  the wire while explicit headers retain precedence. Vendor request content types survive body
  encoding. `request.default` supplies an otherwise absent body; `request.base` fills missing
  structured JSON fields. An explicit raw body remains complete and is not merged.

- **Request deadlines:** `omnisdk.Tuning.RequestTimeout` bounds each outbound request, including
  HTTP response body reads and gRPC response consumption. `omnisdk.DefaultRequestTimeout` is
  **30 seconds**. Zero selects the fallback; a negative duration explicitly disables it.
- **Provider overrides:** `omnisdk.Args.TuningByProvider` (`tuning_by_provider` in JSON) maps provider
  names to `omnisdk.ProviderTuning`. Its fields are `RequestTimeout`, `RetryTries`, `RetryRate`, and
  `MaxPerHost`, and `PageLimit`; zero fields inherit the global `Args.Tuning` value, then library defaults. Keys
  accept `google` or `stackql_unstable_google`; the namespaced entry wins when both exist.
  Overrides apply to provider authentication exchanges as well as resource calls, in document-backed
  and built-in plans. Retry and per-host admission state is shared across a provider's exchanges
  within a run. Retry and per-host concurrency settings govern HTTP exchanges.

Whole-query `Timeout`, `Limit`, `Parallelism`, and buffering settings remain global. Duration values
are Go `time.Duration` values; numeric JSON durations are in nanoseconds.

```go
args := omnisdk.Args{
    Tuning: omnisdk.Tuning{RequestTimeout: 30 * time.Second},
    TuningByProvider: map[string]omnisdk.ProviderTuning{
        "google": {RequestTimeout: 60 * time.Second, RetryTries: 2},
        "aws":    {MaxPerHost: 4}, // inherits the global timeout
    },
}
```

- **Replacement mutations:** `query.Replace` and `query.NewReplace` use the resource's `replace`
  mapping, keeping complete replacement distinct from a partial `update`. `DescribeMutation` accepts
  `"replace"`.
- **Required method parameters:** `omnisdk.DocMethod.RequiredParams` lists required parameters using
  the method's SQL presentation names, including unresolved required server variables. The optional
  `aot.MethodInputs` interface exposes this metadata.
- **Document request metadata:** optional `aot.RequestShape` and `aot.RequestDefaults` interfaces
  expose naming, body schema, and fallback body declarations without changing existing implementers.
  Optional `aot.SigningRequest` retains the declared signing service through endpoint rewrites;
  method `serviceName` overrides document `info.x-serviceName`. Optional
  `aot.BodyTranslationRequest` exposes `requestBodyTranslate`, and `aot.StringOnlySchema` exposes
  `x-stackQL-stringOnly`: nested inputs are coerced against their schema before JSON stringification.
- **OCI authentication:** `omnisdk.Auth` accepts OCI tenancy/user OCIDs, fingerprint, RSA private
  key, passphrase, configuration file and profile, including named environment variables for
  credentials. Set `Type: "oci_signing_v1"`. Explicit raw credentials must be complete; partial raw
  credentials fail instead of falling back to a profile. Document `oci_signing_v1` exchanges use
  the existing request signing and deadline machinery.
- **Read controls:** `Args.ReadHintsByAlias` carries original projection order, sort terms,
  limit, offset and count intent as `ReadHints`/`ReadOrder` DTOs. Optional `ReadEffects` on returned
  rows reports `AppliedOffset()`: a SQL adapter must subtract that offset from its local offset.
  Unsupported filtering, joins, aggregation or ordering prevent unsafe offset pushdown. Existing
  callers that omit hints retain local pagination. Optional `aot.ReadControls` and `aot.OrderSyntax`
  describe document order/skip/count capabilities without extending mandatory interfaces.
- **GraphQL documents:** optional `aot.GraphQLExchange`, `aot.GraphQL`, and `aot.SelectedSchema`
  expose declared queries, continuation and selected row schemas. JSON selection supports property,
  wildcard, array index and slice paths through `pkg/jsonpath.Selection`.
- **Service discovery:** `DocServices` returns `DocService` metadata for all provider declarations,
  including services without installed operation files. `DocCatalog` retains its existing meaning
  of locally addressable exchanges. Colliding service names cannot replace separately keyed services.
- **Document views:** `DocView` exposes an optional `aot.View` with its SQL definition, dialect
  predicate and fallback. It performs no SQL execution. Optional `aot.RequiredSchema` and
  `aot.ParameterDefault` expose required object fields and wire defaults.
- **Document retries:** optional `aot.RetryExchange`/`aot.HTTPRetry` declarations inherit from
  method, resource, then document. They retain the provider's shared retry governor. An explicit
  provider `RetryTries` override wins; global settings supply the fallback when no declaration exists.
- **HTTP errors:** `HTTPStatusError` exposes status, response body and request URL through
  `errors.As`, preserving the distinction between a provider rejection and a transport failure.

## Behaviour changes

XML row selection follows declared array-item element names, including annotations beside `allOf`
references. Scalar comparison calls (`=`, `!=`, `<>`, `<`, `<=`, `>`, `>=`) use the configured SQL
dialect, including NULL propagation. Join equalities on request inputs already supplied by explicit
constants or fanouts constrain rows instead of introducing a second producer for the input.

| Change | Who is affected |
|--------|-----------------|
| Requests without an explicit timeout now have a 30-second deadline instead of being unbounded. Each retry or pagination request gets a fresh deadline. Parent/query deadlines still bound the whole run, and explicit HTTP client timeouts retain precedence. | Callers with slow synchronous APIs: set a longer global or provider `RequestTimeout`. Use a negative duration to opt out explicitly. |
| Cancellation and deadline errors are terminal under the default retry classification. Other configured transient transport errors and statuses remain retryable. | Callers relying on retries after a timed-out request. A stalled default request now returns after 30 seconds rather than repeating until a process guard terminates it. |
| `page_number` pagination sends page 1, then advances the reported current page until it reaches `responseTerminator`. A missing or invalid page/count stops pagination; a non-advancing page before the terminator returns an error. | Documents declaring `algorithm: page_number`: the current page is no longer reused as an opaque next-page token, which could loop on page 1. |
| Pagination configuration is resolved from method, then resource, then document. | Resources declaring `config.pagination`, including Link-header pagination and differently named continuation headers: their declarations are now honoured. |
| A query pagination token with `requestToken.encoding: none` is sent verbatim. Other query parameters retain normal escaping. | APIs issuing tokens such as padded base64 cursors. With no encoding override, normal escaping is unchanged. |

Providers declaring `config.snake_case_aliases` now expose response columns and star projections
in snake case. Native response names remain addressable. Methods declaring `request.nativeCasing`
resolve snake parameter and body keys to the schema's declared wire names. Unknown WHERE symbols
are rejected instead of silently retained as unused parameters; explicit server and signing scope
bindings remain supported.

JSON bodies are coerced against declared schemas before transmission: numeric strings become
numbers, integral values are required for integer properties, boolean strings become booleans, and
scalar values for string properties become strings. Invalid values fail before the resource request.
JSON strings for declared array/object fields are decoded and validated against that shape.
Constant function expressions are accepted as mutation values without requiring a source table.
Undeclared fields are preserved. A declared JSON `request.base` supplies missing fields when callers
provide structured body fields. Explicit raw bodies retain precedence.

### Registry layouts and legacy methods

Provider `resources` references can now resolve resource-only documents alongside their `service`
references. Operation pointers retain their declared `services/`, `resources/`, or `services-split/`
paths and resolve schemas in the operation's own OpenAPI document. Referenced documents are cached
by file size and modification time, and references cannot escape the provider bundle. Byte-only
`Parse` callers still need a catalog to resolve external files.

When a SQL verb has no explicit mapping, legacy method names are supported in this order:
`select`, `list`, `aggregatedList`, `get` for SELECT; `insert`, `create` for INSERT; and `delete`
for DELETE. An explicit empty mapping disables the verb. HTTP verb segments in operation references
are case-insensitive. No new update or replacement methods are inferred.

Document method signatures now include required server bindings. A path/server scope predicate
binds the request without comparing its short identifier against a same-named response URI.
Google service-account credentials accept RSA keys in both PKCS#8 and PKCS#1 PEM formats.

GraphQL cursor, keyset, offset and page-info pagination use the shared HTTP client, signing,
retry and timeout policies. Repeated cursors fail instead of looping. XML continuation paths
are read from the document's transformed response, and singleton rows can sit beside metadata.
OpenAPI `allOf` response schemas retain inherited properties and local overrides within the
existing materialisation depth bound.
Request translation and query transposition inherit from method, resource, then document. Declared
JSON body properties join the request input vocabulary, including SQL `data__` bindings. Returned
rows include explicitly bound path/server values when the response omits those fields; provider
response fields retain precedence.
Source-only WHERE filters run before dependent requests, while optional-side left-join filters retain
their final evaluation. Qualified IN inputs count as known request inputs during dependency analysis.
SQLite WHERE aliases retain source-column precedence, including through SQL staging. Constant false
mutation predicates produce zero rows without sending a write. Tied UPDATE methods prefer a declared
partial update; other ambiguities still fail and explicit method pins retain precedence.
Declared header defaults, including routing headers, are applied when the caller supplies no value.
Path/server scope is still explicit. Method `schema_override` declarations determine response columns;
XML row selection follows nested envelopes before materialisation. Exact MXJ template version
`golang_template_mxj_v0.1.0` is supported alongside the existing versions. SQLite's StackQL function
catalog includes the `string(value)` constructor.

Repeated REST continuation tokens or URLs now return an error rather than looping indefinitely.
Requests use the declared response media type for Accept unless an explicit header overrides it.

## CLI

No new `omnicli` flags. CLI requests inherit the finite request timeout and pagination fixes above.
The existing retry flags do not make cancellation or deadline errors retryable under the default
classification.

Value encoding and output-stream APIs already present in `v0.1.5-beta06` are unchanged by this work.
