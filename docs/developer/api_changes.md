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

Interfaces that gained methods (a caller that only *uses* them is unaffected; one that *implements*
them must add the methods): `MethodSignature.ColumnTypes`, `ParamSignature.In`, `Node.Tuples`,
`query.Target.Rows`.

Additions, no change needed: `Args.AuthByProvider`, `Args.Functions`, `Auth.Location`,
`Auth.Username`/`Password`/`UsernameEnvVar`/`PasswordEnvVar`, `ColumnType`, `EffectError` and the
`ErrRejected`/`ErrOutcomeUnknown`/`ErrNotAttempted` sentinels, `query.NewInsertRows`, `pkg/sqlfn`,
`Auth.TLS`/`Context`/`Successor`/`Subject`/`Profile`, auth type `kubeconfig`. `AnalyzeDocuments` and `omnicli doc-lint`
(package `pkg/docparse/doclint`: document analyzers by format).

### Behaviour changes

| Change | Who is affected |
|--------|-----------------|
| Graph results drop credentials (a service account's signed assertion, bearer tokens) by default. | Anyone reading a token out of a result row. Set `Args.Redaction = omnisdk.RedactNone()`, or pass `--show-credentials` to the CLI. |
| When every node has a projection, a row holds exactly the projected columns; query inputs such as `region` no longer appear in it. | Anyone reading an input back out of a projected row. Project it explicitly. |
| A JSON request-body value that is exactly one placeholder is sent with the bound value's type, so a number or boolean arrives as one, not as its text. | Anyone relying on the API accepting a string where it declares a number or boolean. |
| A failed mutation reports "rejected" for a 4xx; only no response or a 5xx is "may or may not have taken effect". | Anyone matching on the error text. |
| A provider whose document declares `basic`, `bearer` or `custom` auth is now authenticated; with no credential available the query fails at plan time, naming the provider. | Anyone who relied on such calls going out unauthenticated. |
| A document parameter declared by `$ref` is now a parameter; a `$ref` that resolves to nothing is an error. | Anyone whose query worked only because a required parameter was dropped. |
| A document declaring `oauth2` with its own `token_url` exchanges tokens there, not at Azure's login endpoint; an unset `{{ .__env__X }}` in the URL is an error. | Callers of such providers who supplied `AZURE_*` credentials. |
| A server URL's `{name:pattern}` variable is now bound as `{name}`. | Queries on such providers that failed to bind it. |
| A join on a column the other relation takes as an optional parameter is now an edge (one request per left row), not a filter over an unscoped listing. | Joins such as `iam.users` to `iam.access_keys`: they now return every user's keys, and cost one request per user. |
| A wired value that arrives NULL or empty sends no request: the row is unmatched (left join) or dropped (inner join). | Anyone who saw a request go out with a blank parameter. |
| Methods tied on satisfied parameters now prefer those taking no request body; the error lists the tied methods. | Azure `storage_accounts` by `subscription_id` now uses `list`. |
| Pagination is followed: the method's or document's declaration, Microsoft's `x-ms-pageable`, Google's `pageToken`/`nextPageToken`, AWS `Marker`/`NextToken`, and `Link` headers. | Every multi-page list, which previously returned its first page only. |
| Responses declared by `$ref` to `components/responses`, or keyed `2XX`, now have columns. | Microsoft Graph (Entra ID) relations. |
| A list declaring no `objectKey` reads its rows from `$.items` when the response has a list there (the whole body otherwise). | Relations whose rows came back as one envelope row, e.g. `googleadmin.directory.tokens`. |
| S3 requests carry and sign `x-amz-content-sha256`. | All `aws.s3.*` relations. |
| AWS credentials fall back to the shared-config profile (`AWS_PROFILE`, `Auth.Profile`, `credential_process`); Google to gcloud application-default credentials, including user logins; Azure `azure_default` to the Azure CLI. | Interactive users without keys in the environment. |

### CLI: `doc-graph` JSON

| Before | After |
|--------|-------|
| `"addresses": ["a.b.c", …]` | `"nodes": [{"alias": "x", "address": "a.b.c"}, …]` (an omitted alias is the address) |
| `"projections": [{"address": …}]` | `"projections": [{"alias": …}]` |
| — | New: node `verb`, `body` (an object of field → value), `params`; override `poll`; top-level `patches` and `doc_cache`. |
