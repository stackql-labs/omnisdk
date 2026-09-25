# API changes

What callers must change, newest release first. Additions that need no change are not listed.

## Unreleased (after v0.1.3-alpha01)

### Compile-time breaks

| Before | After | Caller change |
|--------|-------|---------------|
| `NewGraph(addresses []string, …)` | `NewGraph(nodes []Node, …)` | `NewGraph(omnisdk.NodesOf(addresses...), …)`. Wirings, projections and overrides naming those addresses keep working: `NodesOf` aliases each node by its address. |
| `NewGraphWithProjections(addresses []string, …)` | `NewGraphWithProjections(nodes []Node, …)` | As above. |
| `Projection.Address()` | `Projection.Alias()` | Rename the call. `NewProjection`'s first argument is now an alias; with `NodesOf` it is still the address. |
| `Graph`, `Node`, `Override` interfaces | New methods (`Nodes`, `Filters`, `Fanout`, `Outputs`; `Verb`, `Body`, `Outer`, `On`; `Poll`) | Only a caller that *implements* these interfaces must add them. Callers that use the constructors are unaffected. |

Kept with a deprecation notice, no change needed yet:

| Deprecated | Use instead |
|------------|-------------|
| `NewFromCatalog` | `NewSelectFromCatalog` |
| `NewGraphQuery` | `NewGraphSelectQuery` |
| `Graph.Addresses()` | `Graph.Nodes()` — one address may now appear under several aliases |

### Behaviour changes

| Change | Who is affected |
|--------|-----------------|
| Graph results drop credentials (a service account's signed assertion, bearer tokens) by default. | Anyone reading a token out of a result row. Set `Args.Redaction = omnisdk.RedactNone()`, or pass `--show-credentials` to the CLI. |
| When every node has a projection, a row holds exactly the projected columns; query inputs such as `region` no longer appear in it. | Anyone reading an input back out of a projected row. Project it explicitly. |
| A JSON request-body value that is exactly one placeholder is sent with the bound value's type, so a number or boolean arrives as one, not as its text. | Anyone relying on the API accepting a string where it declares a number or boolean. |
| A failed mutation reports "rejected" for a 4xx; only no response or a 5xx is "may or may not have taken effect". | Anyone matching on the error text. |

### CLI: `doc-graph` JSON

| Before | After |
|--------|-------|
| `"addresses": ["a.b.c", …]` | `"nodes": [{"alias": "x", "address": "a.b.c"}, …]` (an omitted alias is the address) |
| `"projections": [{"address": …}]` | `"projections": [{"alias": …}]` |
| — | New: node `verb`, `body` (an object of field → value), `params`; override `poll`; top-level `patches` and `doc_cache`. |
