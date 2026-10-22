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

- **Request deadlines:** `omnisdk.Tuning.RequestTimeout` bounds each outbound request, including
  HTTP response body reads and gRPC response consumption. `omnisdk.DefaultRequestTimeout` is
  **30 seconds**. Zero selects the fallback; a negative duration explicitly disables it.
- **Provider overrides:** `omnisdk.Args.TuningByProvider` (`tuning_by_provider` in JSON) maps provider
  names to `omnisdk.ProviderTuning`. Its fields are `RequestTimeout`, `RetryTries`, `RetryRate`, and
  `MaxPerHost`; zero fields inherit the global `Args.Tuning` value, then library defaults. Keys
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

## Behaviour changes

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
Undeclared fields are preserved. A declared `request.base` body is used only when the caller supplies
no body fields.

## CLI

No new `omnicli` flags. CLI requests inherit the finite request timeout and pagination fixes above.
The existing retry flags do not make cancellation or deadline errors retryable under the default
classification.

Value encoding and output-stream APIs already present in `v0.1.5-beta06` are unchanged by this work.
