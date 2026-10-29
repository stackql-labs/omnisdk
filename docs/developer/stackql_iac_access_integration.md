# StackQL agent briefing: IaC and opt-in caller access

Integrate against the current Omni API using [the delta from v0.1.8-alpha02](api_changes.md).
This document is an implementation brief for StackQL agents, not a claim that StackQL's protected
entry points are already wired. The SDK and CLI features below are implemented; the host work is
identified explicitly.

## Ownership of responsibilities

| Concern | Omni owns | StackQL owns |
| --- | --- | --- |
| IaC | Diffing, supported route selection, execution graph, journal/ledger, dependency order, partial deletion | SQL/CLI/transport surface, trusted documents, explicit scope, state-root configuration, error presentation |
| Authentication | Reusable JWT, bearer, mTLS and local-OS verification interfaces | Opt-in configuration, transport evidence, trusted keys/rotation, session lifetime |
| Principal resolution | `principal.Directory` interface and immutable snapshot contract | RDBMS implementation, schema administration, identity/group provisioning |
| Authorization | Session propagation and checks on SDK operations, credentials, collections, relations and transports | Role policy, every non-SDK action, directory administration, SQL statement and function classification |

Keep all RDBMS code and drivers in StackQL. Pass a `principal.Directory` implementation into
`authorization.NewSession`; do not pass a database connection into Omni.

## Integrate IaC without a second DSL parser

1. Use `omnisdk.ParseResources` or `LoadResources` for resource declarations. Remove the duplicated
   `resourceDTO` conversion in StackQL's `internal/stackql/intrinsic/iac.go`: it still accepts
   `params`/`correlationParam`, cannot retain typed binding objects, and diverges from the SDK.
2. Resource `write_once` supplies unreconciled request inputs. `desired` is maintained state.
   `Args.Params` remains execution scope, not the renamed resource object. Pass explicit input and
   environment/context maps through `Args.Inputs` and `Args.Environment`. Preserve JSON numbers
   with `UseNumber`; never evaluate input strings as templates or SQL.
3. Keep keys unique within the collection. Carry sibling IDs with `inbound`/`via`; `self.key`
   is a declaration key, not a cloud identity. Provider `identity` and `addressed_by` describe
   the create response and subsequent addressing contract.
4. Supply `NewLedger(collection, stateDir, runID)` and `DesiredState(resources)` to `Prepare`.
   The collection and cloud scope must be explicit. The host may explicitly configure its local
   state root, then confine caller-selected relative state names beneath it. Reject symlink/path
   escapes and cross-tenant state access. Do not derive collection ownership from cloud tags.
5. Open the returned plan using the caller's execution context, consume rows, inspect `Rows.Err`,
   and close the cursor. Rows with `error`, failed status or `outstanding` must become application
   failures, not a successful SQL result merely because row streaming completed. `applied` can
   also mean no-op. Propagate cancellation/deadlines into all execution and compensation work.
6. Expose partial deletion through `Destroy(keys...)`, full deletion through `Destroy()`. Require
   an explicit nonempty selection when the partial-delete syntax is supplied. All retained
   dependents block selected deletion before delete effects. Update the resource declaration
   after pruning: reapplying its original contents requests recreation. Omission alone does not delete.

Basic host execution (error handling around caller establishment is shown later):

```go
resources, err := omnisdk.ParseResources(resourceJSON)
if err != nil { return err }
args.Caller = caller
args.RequireCaller = true
plan, err := omnisdk.Prepare(registry, omnisdk.DesiredState(resources),
    omnisdk.NewLedger(collection, stateDir, runID), args)
if err != nil { return err }
rows, err := plan.Open(ctx)
if err != nil { return err }
defer rows.Close()
for rows.Next() {
    // Report rows; treat error/outstanding results as failed operations.
}
return rows.Err()
```

When caller protection is disabled, construct the ordinary Args without requiring a caller.
When it is enabled, there must be no branch that executes after authentication or directory failure.
Do not let JSON or SQL predicates set `Caller`, `RequireCaller`, registry trust, verification keys,
authorization rules or privileged endpoint overrides.

Routing recommendations use whole changed sets. `UpdateCandidates` performs discovery, not effects;
`Prepare`/`Open` executes supported desired-state plans. Do not greedily execute one recommendation
per attribute. Current automatic lowering covers eligible same-resource update/replace arms;
arbitrary detach/replace/reattach graphs still need explicit managed declarations. Keep experimental
routing/dependency declarations close to deployment examples; publish reusable resource-type rules
into StackQL documents once semantics are established. Both sources should feed one internal model.
The current runtime reads update recommendations from provider documents; deployment-level routing
is not yet a separately supported JSON field. Use an explicit isolated document overlay today.

Identity cases and limitations: eager IDs, awaited early/late IDs and caller names are exercised by
[the lifecycle examples](../../cicd/examples/identity-lifecycle/README.md). A finished await releases
dependents; an early ID alone does not. Crash-resuming a persisted operation handle remains missing.
Never recover uncertain identity/ownership by searching labels. Surface uncertainty for operator
resolution while preserving the journal and ledger.

## Enable authentication deliberately

Default remains unprotected for compatibility. Add one host-owned switch/configuration that enables
caller protection for the selected deployment. Reject incomplete enabled configuration at startup.
Keep caller credentials distinct from provider credentials and provider `AUTH` statements.

At the transport boundary:

1. Obtain evidence from the real request/connection. A claimed SQL username or a token's decoded
   payload is not evidence. For JWT, pin issuer, service audience, algorithm allowlist and trusted
   verification keys. Resolve `kid` only against that configured key set; never fetch token-provided
   `jku`, `x5u`, issuer URLs or filesystem paths. Bind each key to its permitted algorithm.
2. Call `authenticator.Authenticate(ctx, authentication.Evidence{Token: token})`. Only after success
   call `authorization.NewSession(ctx, identity, directory, authorizer)`.
3. Store the session in a request/connection context, not a package global. Pass `Args.Caller` and
   `RequireCaller: true` to all SDK plans on protected paths. Use `caller.Context(ctx)` for host
   checks through `authorization.Check`, background work and relation callbacks.
4. Authenticate and resolve again at a documented boundary, preferably each request/statement.
   Verification happens when a session is established, not on every emitted row. A long-lived
   connection must not make an expired JWT or revoked membership valid indefinitely. Bound session
   lifetime to token expiry and host policy; explicitly decide how in-flight work is handled.

For example, after constructing a pinned `jwt.Keyfunc`, a directory and an authorizer:

```go
authn, err := authentication.NewJWT(issuer, audience, []string{"EdDSA"}, pinnedKeys)
if err != nil { return err }
identity, err := authn.Authenticate(ctx, authentication.Evidence{Token: token})
if err != nil { return err }
caller, err := authorization.NewSession(ctx, identity, directory, authorizer)
if err != nil { return err }
protectedCtx := caller.Context(ctx)
// Authorize host actions with protectedCtx; execute SDK plans with Caller/RequireCaller.
```

Use a maintained JWT verifier, not custom signature parsing. Require expiry and validate signature,
issuer, audience, not-before and issued-at. Do not use token roles/groups as directory membership.
Issuer-qualified subjects prevent two issuers' identical `sub` values becoming the same account.
Key rotation must be host-controlled and bounded; unknown or removed keys fail closed. Never emit
caller tokens, provider secrets, private signing keys or raw authentication headers in SQL errors,
HTTP logs, traces, journals or Robot artifacts. TLS is required for remote bearer/JWT transport.

## RDBMS principal directory

The inspected StackQL tree already has `internal/stackql/accessmanagement/directory.go`:

- `NewDirectory(engine)` obtains the configured SQL engine's database and returns `principal.Directory`.
- `InitializeSchema(ctx, engine)` is a separate administrative action. It must not run as a side effect
  of authenticating an unknown user or executing an ordinary SELECT.
- Tables: `stackql_access_principals`, `stackql_access_identities`, `stackql_access_groups`,
  `stackql_access_memberships`.
- `(issuer, subject)` uniquely binds a verified identity to a principal. Membership and attributes
  come from one parameterized SELECT, producing a fresh immutable session snapshot. Unknown
  identities, broken membership references, malformed attributes and database errors fail closed.

Keep that adapter host-owned. Provision identity bindings through a separately authorized admin
path. Authentication must never auto-create a principal, group or privileged membership. Use fixed
schema/table names and parameter placeholders, never token or SQL text substitution. Grant the
runtime resolver only the access it needs; schema/membership writes belong to an administration
identity. PostgreSQL can enforce separate DB roles/schema privileges. SQLite requires filesystem
protection and application-level authorization of every SQL path; enable FK enforcement on every
connection and do not expose directory tables as ordinary reader-queryable relations.

Do not let a reader change the directory through arbitrary SQL, writable views, stored routines,
backend escape hatches or implicit schema initialization. Tenant scope, if applicable, must be
part of the directory and collection authorization model, not inferred from a user-provided name.
Avoid a membership cache initially. Existing snapshots stay fixed during their execution; a new
session must see membership changes. Test SQLite and PostgreSQL against the same behavioral contract.
Directory failure is denial, never an invitation to use an OS user, provider account or cached admin.

## Reader role means safe reads, not merely SELECT syntax

A `readers` directory group can receive the implicit host capability to run **safe read operations**.
It must still have explicit permission to use the relevant provider credentials and operation set.
Map the host capability to trusted compiled methods; do not decide that a method is safe from a
`list`/`get` name or HTTP GET alone. Trusted documents and host classification define allowed effects.

The important existing trap is `SELECT ... FROM stackql_preview.iac.converge` or `.destroy`:
these are effectful relations despite SELECT syntax. A reader must be refused both. Apply the same
rule to EXEC, mutations hidden in views/subqueries, local inline commands and side-effecting functions.
Authorize the actual expanded operation graph, including generated dependencies and completion polls,
not just the outer SQL token. Host-owned RDBMS operations and opaque relation callbacks need host
checks; the SDK cannot inspect their implementations.

For a trusted mock method, a minimal SDK reader policy is:

```json
[
  {"subject":"group:readers","resource":"operation/demo/list/listItems","action":"execute"},
  {"subject":"group:readers","resource":"credential/demo","action":"use"}
]
```

`operation/<provider>/<method name>/<operation ID>` uses real document identifiers. Do not grant
`operation/<provider>/**` to readers: it grants mutation methods too. Collection `converge`/`destroy`,
credential reveal, directory administration and document/endpoint reconfiguration are separate
privileges. A policy granting collection access still does not grant the underlying provider methods.

## Host wiring checklist

Inspect these current StackQL integration points rather than patching only the main query path:

| Host area | Required integration |
| --- | --- |
| `intrinsic/iac.go` | Shared SDK resource parser; input maps; scoped state; caller and collection policy; selected deletion |
| `intrinsic/preview_services.go` (`previewArgs`) and `intrinsic/omnisdk.go` | Central caller-bearing Args construction, explicit scope and deadlines |
| `intrinsic/doc.go`, `dynamic.go`, `exec.go`, `operation_completion.go` | Same caller on expanded graphs, native EXEC and completion reads |
| `intrinsic/transaction.go` | Preserve authenticated context for inverse work; explicit compensation policy, never an anonymous retry |
| `intrinsic/*relations.go` | Host authorization before backend reads or mutations; directory tables excluded from ordinary access |
| Transport/session setup | Opt-in authentication, secure evidence, bounded sessions, whoami/groups |
| Legacy/non-Omni execution | Equivalent authorization or explicit refusal while protection is enabled |

The inspected `intrinsic/omnisdk.go` contains an `Open(context.Background())` call: replace that with
an authenticated request context. Audit similar context resets throughout the host. Setting the SDK
session on only the new IaC path does not protect StackQL as an application. SQL identifiers and
backend object permissions must be checked before resolving or executing an opaque RDBMS handle.

## Robot acceptance in both repositories

Here, `test/robot/jwt_access.robot` uses the actual CLI, signed Ed25519/RSA JWTs and a local provider
API. It proves reader SELECT success, mutation/EXEC and IaC denial before provider effects, directory
membership precedence over token groups, operator success, invalid JWT rejection, and credential/
operation permission checks. Its directory is an in-memory snapshot, **not the StackQL SQL adapter**.

Over in StackQL, add transport-level black-box Robot cases using its real configured SQLite and
PostgreSQL directory. Do not mirror Omni internals in expectations or relax existing provider fixtures.
Seed the directory through an admin fixture and assert public results and observed effects:

1. Access disabled: existing supported behavior remains unchanged.
2. Access enabled: no credential, bad signature, wrong issuer/audience, missing/expired expiry,
   future temporal claims and unknown keys fail before any provider/backend mutation.
3. Valid reader: `whoami`/groups and an ordinary SELECT work. Same `sub` from another issuer
   cannot inherit that reader. Token `groups:["admins"]` cannot elevate it.
4. Reader INSERT/UPDATE/DELETE/EXEC and SELECT over IaC converge/destroy fail. Assert no HTTP
   mutation, local command execution or durable mutation intent, not merely an error string.
5. Reader cannot inspect or write directory tables or invoke an indirect path to them. Directory
   outage, absent schema, unknown principal and malformed membership/attributes deny access.
6. Remove membership through the admin fixture: the next statement/session loses access. No
   stale session is reused past its configured lifetime. Exercise two concurrent callers for isolation.
7. An explicitly authorized operator performs create, update several times, partial delete,
   another update and final teardown using the same collection/state. Read-only handles remain
   protected. Try a cross-collection/cross-tenant state handle and an empty prune selection.
8. Repeat an authorized read with cache enabled, then issue it as a denied caller: cached rows
   must not bypass authorization or leak across identities/tenants.
9. Exercise generated dependencies, an operation poll and the legacy fallback path. Every route
   must retain caller context. Invalid authentication must never select an unprotected fallback.

Use short-lived locally signed test JWTs, test-only keys and mock endpoints. Keep tokens out of
command-line arguments and artifacts. Run the same matrix on each supported host transport; a
CLI-only pass is not proof of protected HTTP/Postgres-wire entry points.

For this repository's focused check:

```sh
go build -o build/omnicli ./cmd/omnicli
test/mock/.venv/bin/robot --outputdir test/robot/out/jwt test/robot/jwt_access.robot
```

Use `test/robot/run.sh` for the complete suite. Consult [access management](access_management.md)
for the full permission vocabulary and CLI JWT configuration.
