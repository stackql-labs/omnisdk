# API changes

Migrating from **`v0.1.8-alpha02`** to the current tree. This is the delta from that tag;
features already present there are not listed as additions.

## Required migration

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
