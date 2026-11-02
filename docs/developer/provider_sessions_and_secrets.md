# Provider sessions, tenancy limits and secret stores

A provider binding names one explicitly configured tenancy of a provider. Reuse the provider
metadata while selecting credentials, request scope and limits through independent aliases:

```json
{
  "nodes": [
    {"alias":"east", "address":"production.ec2.vpcs"},
    {"alias":"west", "address":"development.ec2.vpcs"}
  ],
  "args": {
    "provider_aliases": {
      "production": {
        "provider":"aws",
        "tenancy":"production-account",
        "auth": {
          "type":"aws_signing_v4",
          "access_key_id_env_var":"PRODUCTION_AWS_ACCESS_KEY_ID",
          "secret_access_key_env_var":"PRODUCTION_AWS_SECRET_ACCESS_KEY"
        },
        "params":{"region":"ap-southeast-2"},
        "tuning":{"max_concurrent":4}
      },
      "development": {
        "provider":"aws",
        "tenancy":"development-account",
        "auth": {
          "type":"aws_signing_v4",
          "access_key_id_env_var":"DEVELOPMENT_AWS_ACCESS_KEY_ID",
          "secret_access_key_env_var":"DEVELOPMENT_AWS_SECRET_ACCESS_KEY"
        },
        "params":{"region":"us-east-1"},
        "tuning":{"max_concurrent":2}
      }
    }
  }
}
```

Save the graph as `graph.json`. The current CLI accepts its JSON as a string:

```sh
build/omnicli doc-graph test/corpus/registry "$(cat graph.json)"
```

This example makes real AWS reads. Provider configuration and secret sources must be trusted
operator input. Alias names replace only the provider component of a document address; the actual
provider's schemas, methods, signing and operation permissions remain authoritative. Node parameters
can override binding defaults explicitly. A tenancy is a configured scope identifier, not proof of an
AWS account ID: the host can verify account identity with the provider if its policy requires that.
No account, region or credential is inferred from another alias.

A binding needs explicit `provider`, `tenancy` and an `auth` DTO. Nil credentials fail; they do not
borrow the global provider credential. An explicitly supplied auth DTO retains the provider's normal
literal/environment/file resolution semantics. An unresolved `auth_secret` is rejected by direct
planning; resolve it through a client-session snapshot first.

Bindings currently apply to `NewGraphQuery` and `NewSelectFromCatalog`, including their CLI graph
consumer. They do not register new built-in catalog methods, alter SQL parsing, or add multi-tenancy
to the existing desired-state/ledger addressing model. Do not use aliases in IaC until the host has
explicitly modeled provider/tenancy in its durable resource handles.

## Request limits

`ProviderTuning.MaxConcurrent` (`max_concurrent` in JSON) limits in-flight HTTP requests for the
whole `(provider, tenancy)`, across hosts and aliases. `MaxPerHost` remains a separate host limit.
A bound tenancy without an explicit total cap uses the effective per-host limit, defaulting to eight.
Negative `MaxConcurrent` explicitly removes the total cap; host limits remain. Zero on an ordinary
unbound provider adds no new total cap.

Other fields inherit from `TuningByProvider`, then global `Tuning`, then library defaults. Total
request timeout defaults to the existing 30 seconds; this feature does not remove that deadline.
Whole-query limits, time budgets, scheduling and buffer sizes remain query settings. Aliases naming
the same provider/tenancy must agree on their tuning; conflicting declarations fail before effects.
The same pair shares admission and retry state, while unrelated tenancies do not contend on its cap.

Direct Args apply these limits within that execution. To share them across concurrent queries in a
client session, obtain execution Args from the same `ClientSession`.

## Mutable client session, stable executions

```go
store, err := secrets.New(secrets.Config{Path: secretFile})
if err != nil { return err }
session, err := omnisdk.NewClientSession(baseArgs, store)
if err != nil { return err }
err = session.SetProvider("production", omnisdk.ProviderBinding{
    Provider: "aws",
    Tenancy: "production-account",
    AuthSecret: "production/aws-auth",
    Params: map[string]string{"region":"ap-southeast-2"},
    Tuning: omnisdk.ProviderTuning{MaxConcurrent: 4},
})
if err != nil { return err }
args, err := session.Snapshot(ctx, "production")
if err != nil { return err }
plan, err := omnisdk.NewSelectFromCatalog(registry, "production.ec2.vpcs", args)
if err != nil { return err }
rows, err := plan.Open(ctx)
```

A stored auth value is a JSON-encoded `omnisdk.Auth` object. The store path and secret name are
explicit. `Snapshot(ctx, aliases...)` captures selected bindings; no aliases means all. A missing
selected alias or failed secret lookup fails the snapshot. Selecting aliases avoids loading unrelated
credentials. Secret-store I/O happens after releasing the session mutex, before planning provider calls.
Each snapshot owns its DTO data. Earlier snapshots and in-flight plans keep their credentials when
the session is changed; future snapshots receive the update.

Session controls:

| Method | Effect |
| --- | --- |
| `SetProvider(alias, binding)` | Add/replace a binding; provider, tenancy and alias must be explicit |
| `UnsetProvider(alias)` | Remove that binding |
| `SetAuth(alias, Auth)` | Replace its in-memory auth DTO and remove any secret reference |
| `PatchAuth(alias, map[string]any)` | Atomically set known Auth JSON fields; reject unknown fields/types |
| `UnsetAuth(alias, fields...)` | Clear the named fields; with no fields, remove auth so planning fails until reconfigured |
| `Snapshot(ctx, aliases...)` | Supply detached execution Args with shared session request governors |

Field patches do not evaluate expressions. They never write a secret store implicitly. To edit a
secret-backed credential in memory, explicitly replace it with `SetAuth` first; patches will not
silently materialize and override the persistent source. Clearing one field retains any other
configured environment/file sources; clearing the whole DTO disables that binding rather than
falling back to the global provider credentials. Store writes/deletes are separate explicit actions.

Credential changes retain shared request governors. Structural binding/tuning changes establish
new policies for subsequent snapshots; existing executions finish with their captured policies.
Do not mutate returned Args concurrently with plan construction. Existing provider authentication
code may resolve explicit env/file sources while planning, so inline DTOs or store values are the
way to capture credential material itself in the snapshot.

Alias read-cache partitions include the explicit caller partition, provider, tenancy, alias and
session configuration generation. Session mutations therefore cannot reuse the earlier generation's
cached rows. A caller partition is still required: tenancy is not caller identity or a permission grant.
A changed store value observed by Snapshot also advances the cache generation, without putting
credential material in cache keys. Changes to env/file credential sources outside the Store require
a fresh caller partition or session reconfiguration. Authorization runs before cached reads as it does for live reads.

Protected callers also require `tenancy/<provider>/<tenancy>` action `use`, in addition to ordinary
operation and provider-credential permissions. Hosts must authorize session configuration and secret
access themselves; knowing an alias or secret name does not authorize changing or reading it.

## Public secret storage

`public/secrets.Store` is a small contract: `Get(ctx, name)`, `Set(ctx, name, bytes)`, and
`Delete(ctx, name)`. Hosts can pass their own implementation, including an RDBMS or commercial
vault adapter. Implementations must detach Get results and any retained Set input values. No database connection or driver enters Omni. `ErrNotFound` and `ErrUnavailable`
allow hosts to distinguish absent data from unavailable OS facilities.

`secrets.New(Config{Path: path})` defaults to the file backend. It creates no implicit home path.
Values are base64 JSON data, **not encrypted at rest**. The file is private (0600 on Unix), writes use
an fsynced temporary file and atomic replacement, and directory entries are synced where supported.
Readers get independent byte slices. Each store uses a mutex; an exclusive sidecar serializes writes across other store instances and
processes. Existing corrupt/public/symlink store files are
refused, not overwritten. Use a private operator-owned parent directory on a local filesystem, including appropriate
Windows ACLs. A crashed writer can leave `<path>.lock`; remove it only after confirming no writer
is active. Lock waits honor cancellation and have a 30-second bound.

Opt into OS protection explicitly:

```go
store, err := secrets.New(secrets.Config{Backend:"os", Namespace:"stackql/client-session"})
```

- macOS: generic passwords through the installed `security` tool and the user's Keychain.
- Linux: Secret Service through installed `secret-tool`; its service/session must be accessible.
- Windows: current-user generic credentials through Credential Manager. Values are limited to
  the Windows generic credential blob limit of 2560 bytes.

No fallback to file storage occurs when the OS backend is requested but unavailable. Constructors
check tool/API availability; operations can still fail if the service is locked or unavailable.
Command-based backends send secret data through stdin, never process arguments, and suppress tool
output from errors. Calls honor cancellation and a 30-second command deadline. Keys/values are
encoded as data, never executed as shell fragments. Actual OS service access is an operator action;
automated tests use fake tools and do not modify the developer's keychain.

## StackQL and parser integration

The session API is shared; SQL grammar belongs to the parser/host. This repository does not add
`SET`/`UNSET` SQL productions. Existing StackQL auth configuration includes its separate legacy
`AuthConfig`/`AuthCtx`; translate that to the execution `Auth` DTO through the existing host adapter,
rather than changing legacy field names silently.

Host integration requirements:

1. Own one ClientSession per authenticated client/connection, not a process-global mutable auth map.
2. Parse structured provider-binding, auth-field and secret operations. Validate names/types and
   authorize them before mutation. Apply SET/UNSET at statement execution, not during preparation.
3. Map provider-binding statements to `SetProvider`/`UnsetProvider`, auth statements to the Auth
   setters above, and secret statements to the injected Store. Secret mutation and session mutation
   are separate operations; never persist a credential just because the caller set a session field.
4. Resolve an alias's metadata through its canonical provider while retaining the alias and tenancy
   in the execution binding. Feed the chosen aliases to Snapshot, and carry Args through every
   generated dependency, poll and query execution path. Preserve caller context and explicit scope.
5. Keep the persistent principal directory separate from provider credential storage. An RDBMS
   secret-store adapter and its schema/permissions remain host-owned. Secret get/set/delete, provider
   binding edits, auth edits and secret revelation require distinct host permissions.
6. Add parser round-trip tests and host Robot tests that set/unset auth inside one session, compare
   two simultaneous sessions, rotate credentials during an in-flight query, and query two accounts.
   Check actual signatures/scopes and observed concurrency; do not test only statement acceptance.

See [the access integration briefing](stackql_iac_access_integration.md) for transport protection and
RDBMS principal resolution. The sibling StackQL and parser feature branches implement SQL configuration and UNSET normalization.
See StackQL `docs/provider_sessions.md` and parser `SESSION_CONFIGURATION.md`. StackQL continues
to consume tagged dependencies; the parser changes need their own release tag before a standard
StackQL build accepts UNSET. SET ... = DEFAULT uses the existing AST and remains available.

## Verification

`test/robot/provider_aliases.robot` covers real CLI account selection, no credential fallback,
conflicting same-tenancy limits and unresolved secret rejection. SDK race tests additionally cover
concurrent queries sharing account limits, independent accounts, cache separation, auth mutation,
explicit tenancy permission, detached snapshots and file persistence. OS tool tests verify stdin
transport without using real secret stores. The Windows backend is cross-compiled; live Windows
Credential Manager and macOS/Linux service validation remain platform acceptance checks.
