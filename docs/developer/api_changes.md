# API changes

Migrating from `v0.1.5-alpha03` to the current tree.

## Compile-time breaks

None.

## Additions

No caller change needed.

- **Value encoding:** package `pkg/encode` — `Text`, `JSON`, `AsInt64`, `NonFinite` — the one
  rendering of a cell: never exponent form, a `json.Number` verbatim, keys sorted, no HTML escaping,
  NaN and infinities as null. A host rendering rows itself calls `Text` and matches every sink.
- **Output streams:** package `pkg/stream` — `NewJSONL`, `NewDelimited` (csv, tsv, any single-rune
  delimiter, RFC 4180 quoting), `NewOTelLogs` (OTLP/JSON, int64-fitting numbers as `intValue`),
  `NewTable`; `Options.BatchSize` and `Options.FlushInterval`; `Copy(w, rows)`, which writes what was
  produced before returning a failure. `omnisdk.Rows` is a `stream.Source`.

## Behaviour changes

| Change | Who is affected |
|--------|-----------------|
| A JSON number in a response is kept as the text the provider sent (`json.Number`), not a `float64`: a 64-bit id — an AWS account, a GCP project number — is exact, and `12000000` is not `1.2e+07`. | A caller type-asserting a row value to `float64`: assert `json.Number`, or render through `encode.Text`. |
| An integral provider number is an SQLite `integer`, as SQLite's own JSON functions read one; it was a `real`. | Queries on `typeof(x)`, or on its text: `5`, where it was `5.0`. |
| Every output renders values through `pkg/encode`: rows from the engine's JSONL encoder, a document value's bytes, a map or array as SQL text (SQLite and Postgres), a template's `toJson`. `<`, `>` and `&` are no longer escaped, and NaN and infinities are null rather than an error. | Anyone matching `<`-style escapes, or relying on a NaN failing the row. |

## CLI

| `v0.1.5-alpha03` | Now |
|------------------|-----|
| Rows, `doc-lint` findings and discovery output written by `encoding/json` | Written by `pkg/encode`: nothing HTML-escaped, no exponent form, 64-bit numbers exact. |
