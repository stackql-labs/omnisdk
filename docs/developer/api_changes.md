# API changes

Migrating from `v0.1.3-alpha05`, the version stackql consumes, to the current tree.

## Compile-time breaks

| `v0.1.3-alpha05` | Now | Caller change |
|------------------|-----|---------------|
| `Args.Auth *Auth` | removed | Put the credential under its provider in `Args.AuthByProvider`: `AuthByProvider: map[string]*omnisdk.Auth{"aws": a}`. A key is the namespaced name (`stackql_unstable_github`) or the document's (`github`); hand-authored methods use `aws`, `azure` (Entra included) and `google`. There is no query-wide credential: a provider with no entry authenticates from its document's defaults and the environment, never with another provider's entry. JSON `"auth"` is gone; use `"auth_by_provider": {"<provider>": {…}}`. |
| `NewFromDoc(doc, resource, args)` | `NewFromDoc(doc, provider, resource, args)` | Name the provider whose credentials the call uses; a document alone does not say. |
| `Graph` interface | Gains `Terminations()`, `Branches()`, `Gates()` | Only a caller that implements `Graph` must add it; graphs from `NewGraph` are unaffected. |

## Additions

`ConvergeGraph`, `NewRecallNode`, `NewDiffNode`, `Managed`/`NewManaged`, `DiffAbsent`/`DiffMatch`/`DiffDrift`; `Branch`, `Arm`, `Gate` with `NewBranch`, `NewArm`, `Otherwise`, `NewGate`, `WithBranch`; `Termination` with `Rounds`, `Records`, `Within`, `AnyOf`, `AllOf`, and `WithTermination`; `Tuning.RowsAhead`, `Tuning.PagesAhead`, `Auth.Profile`, `Auth.Subject`, `AnalyzeDocuments`, package
`pkg/docparse/doclint`.

## Behaviour changes

| Change | Who is affected |
|--------|-----------------|
| A join on a column the other relation takes as an optional parameter is an edge (one request per left row), not a filter over an unscoped listing. | Joins such as `iam.users` to `iam.access_keys`: they return every user's keys, and cost one request per user. |
| An input with several sources is satisfied by any one, the first with a value winning. A required input with no value sends no request: the row is unmatched (left join) or dropped (inner join). An optional input with no value is left off the request, never sent blank. | Anyone who saw a request go out with a blank parameter, or relied on an empty optional input skipping the request. |
| An inner join on an equality neither table takes as a parameter runs as a hash probe: the later table is listed once and looked up by the earlier one's value, not filtered over every pair. | No change in rows; joins over large tables cost far less. |
| Methods tied on satisfied parameters prefer those taking no request body; the error lists the tied methods. | Azure `storage_accounts` by `subscription_id` uses `list`. |
| Pagination is followed: the method's or document's declaration, Microsoft's `x-ms-pageable`, Google's `pageToken`/`nextPageToken`, AWS `Marker`/`NextToken`, and `Link` headers. | Every multi-page list, which returned its first page only. |
| Responses declared by `$ref` to `components/responses`, or keyed `2XX`, have columns. | Microsoft Graph (Entra ID) relations. |
| A list declaring no `objectKey` reads its rows from `$.items` when the response has a list there (the whole body otherwise). | Relations whose rows came back as one envelope row, e.g. `googleadmin.directory.tokens`. |
| S3 requests carry and sign `x-amz-content-sha256`. | All `aws.s3.*` relations. |
| AWS credentials fall back to the shared-config profile (`AWS_PROFILE`, `Auth.Profile`, `credential_process`); Google to gcloud application-default credentials, including user logins; Azure `azure_default` to the Azure CLI. | Interactive users without keys in the environment. |
| Rows are produced at most a bounded distance ahead of the reader (`Tuning.RowsAhead`, default 1024; `Tuning.PagesAhead`, default 2; negative is unbounded). A slow reader holds the run back rather than growing memory. | A caller that abandons `Rows` without `Close`: the run waits rather than finishing in the background, and is cancelled once the cursor is garbage-collected. |
| Hand-authored Google methods ask for `Auth.Scopes`, defaulting to `cloud-platform`; each asked for a fixed read-only scope. A document reads only the credential its scheme declares. | Callers of `gcp.*`/`google.*` methods relying on a read-only token; the identity's roles still limit what it can do. |
| `Converge` runs keys as a dependency graph: a key starts once the keys it reads from are live, and up to `Tuning.Parallelism` (default 16) converge at once, sharing `Tuning.MaxPerHost`. Declaration order no longer matters; a dependency cycle is refused before anything is sent. The first failure stops new keys; keys in flight finish, then the run unwinds. | IaC callers who relied on declaration order, or on one key at a time. |
| A node may be wired to itself, and nodes to each other: the cycle runs to a fixpoint under a termination checked well-founded at planning. A param and an arrival from the node's own cycle may share a name — the param is the first value. A cycle without a well-founded termination is refused before any request. | Graphs that relied on a self-wiring being rejected. |
| `NaN` compares as text, not as a number; it equalled every number. | Filters or joins comparing a value spelled `NaN`. |

## CLI

| `v0.1.3-alpha05` | Now |
|------------------|-----|
| `doc-select <doc> <resource>` | `doc-select <provider> <doc> <resource>` |
| `"auth": {…}` in args JSON | `"auth_by_provider": {"<provider>": {…}}` |
| — | `--rows-ahead`, `--pages-ahead`, `doc-lint <dir> [provider...]`; `doc-graph` node `terminate` (`rounds`, `records`, `within`) and `branches` |
| `--parallelism` bounds query fan-out | It also bounds IaC keys converging at once. `iac-apply` takes `tuning` from its spec, the flags filling what the spec leaves unset. |
