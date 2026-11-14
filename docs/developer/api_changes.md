# API changes

Migrating from **`v0.1.8-alpha02`** to the current tree. This is the delta from that tag;
features already present there are not listed as additions.

## Required migration

Method discovery: optional `SchemaMethodSignature` exposes recursive request/row schemas;
`DescribeMethodFields` adds bounded depth/path views. Existing `MethodSignature` implementers
remain compatible. See [method_schema.md](method_schema.md) for host integration and source-schema errors.

Supplying `Args.MutationCompletion` suppresses config-based self-read waits, giving the host
ownership of completion. Explicit document beta
edges are unchanged; managed IaC keeps its readiness waits. This requires a release after
`v0.1.11-alpha06`.

Read-only ledger inspection: `ReadLedger(ctx, collection, stateDir)` returns resource keys,
identities, phases, versions and addressing metadata without cloud calls or filesystem writes.
`NewLedger(...)` also exposes this through an optional `Read(context.Context) ([]Row, error)`
capability. Pending identities are prior committed identities. Intent bodies and provider parameters
are omitted; reads are per-entry consistent, not a collection-wide snapshot. This addition is not
in `v0.1.11-alpha01`.

| Surface | Change | Caller action |
| --- | --- | --- |
| IaC resource JSON | `params` becomes `write_once`; `params` is rejected, without an alias | Rename the resource-level object. Execution-scope `Args.Params` / `args.params` and graph-node `params` are unchanged |
| Correlation JSON | `correlation_param`, `correlationParam` and `debug_correlation_param` are rejected | Declare ordinary tag/label data with structured references if wanted; otherwise omit it |
| `ManagedResource` implementations | `CorrelationParam() string` is replaced by `DebugCorrelationParam() DebugCorrelationParam` | Implement the latter returning the zero DTO; remove calls to the former |
| `NewResource` | Signature length/types are unchanged; its final string no longer enables stamping | Pass `""`. A nonempty legacy debug target fails during preparation |
| Debug configuration in Go | `DebugCorrelationParam` and `WithDebugCorrelationParam` retain migration validation only | Nonzero values are rejected; use ordinary `desired` or `write_once` data |
| `Destroy` function type | `Destroy()` becomes `Destroy(keys ...string)` | Existing calls still compile; update stored `func() Statement` references or wrap the call |
| `Args` literals | New fields listed below | Keyed literals remain compatible; update unkeyed literals |

The preview deliberately removes the misleading IaC field names rather than retaining aliases.
The Go `ManagedResource.Params()` accessor and `NewResource` map argument still supply the values
represented by `write_once` in JSON; that interface method has not been renamed.

## Caller access management: opt-in

New public packages:

- `public/authentication`: `NewBearer`, `NewJWT`, `NewMTLS`, `NewLocalOS`, `Authenticator`,
  `Evidence` and provisioned bearer `Token` DTOs. Verified identities are issuer-qualified.
- `public/principal`: immutable `Principal` snapshots, `Identity`, `Directory`,
  `NewPrincipal`, `NewIdentity`, and `NewMemoryDirectoryFromBytes`.
- `public/authorization`: `NewCasbin`, `Rule`, `Authorizer`, `NewSession`, `Session`,
  `Check` and `ErrDenied`.

`Args.Caller` accepts an authenticated session; `Args.RequireCaller` refuses an execution without
one. These are Go-only fields (`json:"-"`), not user-deserializable identity assertions. A caller can
also provide the session through `Session.Context(ctx)` when opening a plan. `Args.Principal`
is not authentication evidence. Existing unprotected executions remain unchanged unless the host
opts in; enabled hosts must require a session on every relevant path and never fall back after failure.

Enforcement covers document operations, generated dependencies/polls, provider credential use,
native exchanges/transports, relation reads and collection converge/destroy. Denied mutations are
not attempted and do not record mutation intent. Cached reads retain authorization checks.
Provider authentication remains separate from caller authentication.

CLI additions: `--access-config`, `--caller-token-env`, `whoami`, `groups`. Mechanisms are static
`bearer`, local `os`, and `jwt`. JWT configuration pins issuer, audience, algorithm allowlist and
`kid`-indexed PKIX public keys; the CLI supports RS256 with RSA keys of at least 2048 bits, and
EdDSA with Ed25519 keys. Tokens come from the named environment variable. Expiration is required;
signature, issuer, audience and temporal checks precede directory resolution. Token group claims
do not supply directory membership. Key rotation and session lifetime remain host responsibilities.

See [access management](access_management.md) and the
[StackQL agent integration briefing](stackql_iac_access_integration.md). The RDBMS directory belongs
to StackQL; Omni adds no SQL directory implementation or database driver.

## Resource bindings

New `Args.Inputs map[string]any` and `Args.Environment map[string]any` provide explicit binding
sources. `desired` supports references recursively; `write_once` values accept references that
resolve to strings:

```json
{"from":"self", "attribute":"key"}
{"from":"input", "attribute":"vpc_name"}
{"from":"env", "attribute":"AWS_REGION"}
```

`self` exposes `key`, `collection`, `provider`, `address`, `desired` and `write_once`.
Dotted attributes are supported, with exact-key lookup first. JSON types are preserved in desired
state. Missing bindings, malformed references and cycles fail before mutations. Strings are not
interpolated. API identities continue to travel through `inbound`, not `self`.

`ResourceEnvironmentVariables(resources)` enumerates explicit environment references without
reading the process environment. The SDK consumes only supplied context. The CLI captures only
referenced environment names, respecting already-supplied `args.environment`, and adds
`iac-apply --input '{...}'` / `--input @file`. Matching input keys resolve in this order:
CLI input, top-level file `inputs`, then `args.inputs`.

See [resource bindings](resource_bindings.md).

## Declared update routing

New optional AOT interfaces `aot.UpdateRouting` and `aot.UpdateRoute` expose validated document
recommendations. Existing catalog implementations need not implement the optional interface.
Declarations can live at document/resource/method scope and retain source/order information.

New `omnisdk.UpdateCandidates(ctx, registry, changes, args)` returns `[]UpdateCandidate` without
provider effects. CLI `doc-update-routes <registry> <address> '["attribute"]'` exposes discovery.
Rules match the complete changed set: `requires ⊆ changes ⊆ permits`. Candidates are ranked
and ordered deterministically; they are recommendations, not a greedy sequence of mutations.

Desired-state execution lowers eligible same-resource update/replace arms, checking policy and
available inputs. An unsupported whole change set fails explicitly. Arbitrary cross-resource
lifecycle synthesis remains unimplemented; supply an explicit managed graph for such plans.
See [route schema](../schemas/update_routes.schema.json) and
[the runnable routing example](../../cicd/examples/update-routing/README.md).

## IaC lifecycle behavior

- `write_once` supplies request inputs outside reconciliation; `desired` declares maintained state.
  Update-only desired fields are excluded from the create arm, and non-null update-only intent
  fails if the object is absent rather than being silently dropped.
- Read/update/delete inputs are filtered against their declared operations. Dependency values no
  longer overwrite the object's own recorded identity when deleting it. Explicit arrival mappings
  take precedence over the older inferred dependency-parameter fallback.
- Automatic correlation tagging is removed from built-in blueprints. Only caller-declared tags
  are sent. The ledger and journal remain authoritative; cloud labels do not recover ownership,
  intent or permission to adopt objects.
- Awaited operations preserve early response fields missing from the completed response;
  completed values take precedence. Dependents still wait for successful completion. A persisted,
  resumable polling handle after process death is not implemented.
- `Destroy(keys...)` / repeated `iac-destroy --key KEY` selectively remove resources. All selected
  keys and retained dependencies are checked before deletes; selected dependents go first.
  Empty `--key` cannot fall through to full teardown. No keys retains full collection destruction.
  Omitting a resource from an apply file alone does not prune it.
- Managed-graph validation aggregates undeclared mutations and rejects mutations of read-only
  relations. Diff evaluation uses the execution context for cancellation and policy decisions.

The [identity lifecycle walkthroughs](../../cicd/examples/identity-lifecycle/README.md) cover eager
provider IDs, awaited early/late IDs, caller-named objects, repeated updates, partial deletion,
local key generation and teardown. Robot checks are black-box CLI tests against local APIs;
they do not establish that the live cloud walkthroughs have been executed.

## Provider tenancy sessions and secret storage

`Args.ProviderAliases` binds explicit aliases to canonical providers, tenancy identifiers, auth DTOs,
parameter defaults and `ProviderTuning`. Document graphs and addressed SELECTs support these aliases.
New `ProviderTuning.MaxConcurrent` limits total HTTP requests per provider/tenancy across hosts.
It is separate from `MaxPerHost`. Protected alias execution requires `tenancy/<provider>/<tenancy>`
`use` permission in addition to the existing provider and operation checks.

`NewClientSession(baseArgs, store)` returns `ClientSession` with provider and auth SET/UNSET methods
and detached `Snapshot(ctx, aliases...)` Args. Concurrent snapshots share account admission/retry
state. Session credential edits affect future snapshots, retain existing governors and invalidate
their read-cache generation. The SDK adds no SQL SET/UNSET grammar or persistent side effects to
these in-memory setters.

`public/secrets` supplies the `Store` interface, explicit file-default/OS-opt-in `Config`, `New`,
`ErrNotFound` and `ErrUnavailable`. File values are not encrypted. OS storage uses Keychain,
Secret Service or Windows Credential Manager without a silent fallback. Host RDBMS/vault adapters
implement the same interface without adding database dependencies to Omni.

See [provider sessions and secrets](provider_sessions_and_secrets.md) for precise lifecycle,
permission, cache, platform and host-integration behavior. These additions extend `Args` and
`ProviderTuning`; update any positional DTO literals.

## Opt-in server and OAuth endpoint selectors

New data-only `ServerVariableSpec`/`EnvVarSelector` declarations and optional `ServerVariableSpecs`
metadata retain `x-stackQL-envVarSelector`. `NewServerVariableResolver` returns the resolver
interface; `DocMethod.ServerVariables` exposes copied metadata for host discovery. Existing
`ServerEnvironmentBindings` and `ServerTransportDefaults` contracts are unchanged.

`AuthDefaults.TokenURLVariables` retains annotated `config.auth.variables`. Document OAuth token
URLs resolve these placeholders before existing environment-template expansion. Runtime token URLs
remain authoritative. OpenAPI server defaults remain unapplied, and hosts must explicitly bind
selector-resolved server inputs. See the developer guide's endpoint-selector section.

SQL resolution now retains the origin of query-wide explicit inputs when binding provider aliases.
Those inputs and SQL IN fan-out take precedence over alias defaults, as node-local inputs already
do; ordinary Args.Params defaults retain their existing lower priority.

## Automatic IaC diagnostics

Managed executions now fsync dispatch and success/failure evidence to
`<state>/journal/<run-id>.diagnostics.jsonl`. Records identify the resource, exchange, stage,
outcome, timestamp and error; common provider error codes/request IDs are extracted when available.
No HTTP logging flag is needed. Request/response dumps are not written. These files are separate
from forward journals and never become compensation steps. A killed process can leave dispatch/await
evidence without a terminal outcome; the ledger remains authoritative for uncertainty.

## Explicit pending-outcome recovery

`Prepare(registry, Recover(resourceKey, outcome, identity, version), NewLedger(...), args)` records
an operator-verified `absent` or `applied` outcome without provider calls. Recovery requires the
observed pending version, locks the resource, journals the assertion before the CAS update, and
refuses stale decisions. `absent` cannot discard prior committed state or retained dependents.
`applied` requires a verified completed effect and identity; it never replaces a prior identity.
The ledger configuration also exposes an optional `Recover(ctx, resource, outcome, identity,
version, args)` capability for consumers. This addition requires a new release after `v0.1.11-alpha03`.

## Inherited execution authorization

`authorization.Permission`, `authorization.Authorize`, and `omnisdk.WithPermissions` provide a
shared execution gate. Protected undeclared/incomplete operations fail closed before factories or
mutation intent. Exact grants flow down the invocation context and are reset when the caller changes.
Internal exchange wrappers now inherit an `Access()` declaration; the common executor enforces it
for roots, stages, cycles and admission alternatives. Bespoke document/native/mutation check loops
are removed. Direct ledger reads now require `collection/<name>:read`; sleep and application table
functions require `intrinsic/sleep:execute` and `function/<name>:execute`. Legacy unprotected callers
are unchanged. A new SDK release is required before consumers receive this behavior.

### Explicit AWS STS roles

`Auth.AssumeRole` (`AWSAssumeRole`) supports role ARN, external ID, session name,
duration, explicit STS region/endpoint and source Auth. Acquisition and expiry
refresh occur at request dispatch, coalesced per client session, with independent
caller cancellation. Static AWS authentication remains unchanged. See
[configuration and fixture](provider_sessions_and_secrets.md#aws-assumerole).

### Provider transaction lifecycle

`NewTransactionSession` exposes authenticated, session-bound transaction handles,
shared IaC durability/locking, deferred barriers, commit/rollback and explicit
version-fenced resume/recovery. See [transactions](transactions.md) for current limits.
