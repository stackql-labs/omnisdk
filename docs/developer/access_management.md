# Caller access management

Caller authentication is separate from provider authentication. A provider credential does not grant
its holder permission to execute operations. Existing applications remain unchanged unless they
explicitly enable caller access management.

The public packages are `public/authentication`, `public/principal`, and `public/authorization`.
Authentication returns an issuer-qualified identity. A directory resolves it to an immutable principal
snapshot. A Casbin authorizer evaluates that principal and its directory-owned groups; policy does
not maintain a second membership graph.

## Library use

```go
identity, err := authenticator.Authenticate(ctx, evidence)
if err != nil { return err }
caller, err := authorization.NewSession(ctx, identity, directory, authorizer)
if err != nil { return err }
plan, err := omnisdk.NewGraphQuery(registry, graph, omnisdk.Args{
    Caller: caller,
    RequireCaller: true,
    // Provider credentials and scope are configured separately.
})
if err != nil { return err }
rows, err := plan.Open(ctx)
```

Alternatively, pass `caller.Context(ctx)` to `Open`. `RequireCaller` rejects a missing session before
execution or ledger access. The old `Args.Principal` string is not authentication evidence and grants
no permissions. Application entry points must require authentication when access is enabled; do not
silently retry a failed authentication or directory lookup as an unprotected request.

Authentication implementations:

- Static bearer credentials: `NewBearer` accepts SHA-256 digests of independently generated,
  high-entropy tokens. This is not a password-storage scheme. Comparison uses constant-time digest
  comparison, and errors never contain the submitted token.
- JWT: `NewJWT` requires an issuer, audience, algorithm allowlist, and key resolver. Signature,
  expiration (required), not-before, and issued-at validity are checked using golang-jwt. Key loading
  and rotation belong to the application; no discovery or token issuance is performed.
- mTLS: `NewMTLS` validates client certificates against explicit roots with client-auth usage before
  invoking the application's identity mapper. Supply the real, completed connection's TLS state;
  never construct it from client headers. Certificate revocation is an application/transport concern.
- Local OS: `NewLocalOS` resolves this process's effective UID (current user SID on Windows). It accepts no username or remote
  evidence. The issuer should distinguish machines/authentication domains. Group membership comes
  from the configured directory, rather than being silently imported from the OS.

Directories and policies are snapshots for each established session. A subsequent session resolves
current membership; an existing session does not change underneath concurrent operations. Establish
new sessions at the application's chosen request/session boundary. JWT validity is checked when the
session is established; applications control session lifetime.

## Policy vocabulary

`authorization.NewCasbin` takes JSON-compatible `Rule` values. Subjects are `user:<principal ID>` or
`group:<directory group>`. Resources use Casbin glob matching: `*` matches one path segment and `**`
can span segments. Actions match exactly, or use `*`. Rules grant access; absent permission denies.

| Resource | Action | Boundary |
| --- | --- | --- |
| `command/omnicli/<command>` | `execute` | CLI entry point, including discovery commands |
| `operation/<provider>/<exchange name>/<operation ID>` | `execute` | Each compiled document operation, including generated dependencies and completion polls |
| `credential/<provider>` | `use` | Use of that provider's configured credentials |
| `credentials` | `reveal` | CLI `--show-credentials`, separate from credential use |
| `native/<provider>/<exchange name>` | `execute` | Hand-authored catalog exchange |
| `transport/http/<host[:port]>` | HTTP method | Native/unclassified HTTP request |
| `transport/grpc/<target>` | RPC method | Native/unclassified gRPC request |
| `relation/<address>` | `read` | Application-supplied relation handle |
| `collection/<name>` | `converge` or `destroy` | Durable collection execution |

Exchange names and operation IDs are the document's identifiers, not necessarily SQL resource names.
Direct document queries supply their explicit provider to compilation. Internal bare document
compilation without a provider uses the literal `unscoped` permission namespace; no provider is
inferred. A document operation's authorization covers its own transport requests; its separately
compiled dependencies require their own authorization. Native transports also require explicit
transport permissions. Provider token exchanges require provider credential permission.

Mutation permission is checked before writing its intent to a journal, and again at the document
exchange boundary. A denied mutation is classified as not attempted. Policy errors fail closed.
Read caches do not bypass document authorization. A host must separately authorize operations that
it performs itself, including database administration and SQL inside opaque relation handles.

Provider documents, endpoint overrides, and custom relation implementations are trusted application
configuration. Do not let an untrusted caller replace an allowed document with arbitrary executable
content or redirect an allowed operation to an arbitrary endpoint.

## Omni CLI

All commands accept `--access-config <file>` and, for bearer authentication,
`--caller-token-env <environment-variable-name>`. The token is read from that named variable, not a
command-line argument. The CLI reads the file; constructors perform no filesystem access.

Inspect the authenticated directory identity and memberships with:

```bash
omnicli whoami --access-config access.json --caller-token-env OMNI_CALLER_TOKEN
omnicli groups --access-config access.json --caller-token-env OMNI_CALLER_TOKEN
```

Outputs are JSONL: `{"principal":"alice"}` and `{"groups":["readers"]}`. An empty
membership list is `{"groups":[]}`. Both require a configured authenticated session and their
respective `command/omnicli/whoami` or `command/omnicli/groups` `execute` permission. They never
infer an OS or provider identity when access management is disabled. The same commands work
with an OS-authentication configuration without `--caller-token-env`.

### Verify locally with OS authentication

From the repository root on macOS or Linux, paste the following. It builds the CLI and creates a
temporary configuration bound to your effective OS user. `demo` is a directory group declared by
this example, not a discovered OS or cloud-provider group. No provider credentials are needed.

```bash
omni_check_dir=$(mktemp -d)
go build -o "$omni_check_dir/omnicli" ./cmd/omnicli

python3 - "$omni_check_dir/access.json" <<'PYCONFIG'
import json, os, pwd, sys

name = pwd.getpwuid(os.geteuid()).pw_name
config = {
    "issuer": "local-terminal",
    "mechanism": "os",
    "directory": {
        "groups": ["demo"],
        "principals": [{
            "id": name,
            "identities": [{
                "issuer": "local-terminal",
                "subject": str(os.geteuid())
            }],
            "groups": ["demo"]
        }]
    },
    "rules": [
        {"subject": f"user:{name}", "resource": f"command/omnicli/{cmd}",
         "action": "execute"}
        for cmd in ["whoami", "groups"]
    ]
}
with open(sys.argv[1], "w") as f:
    json.dump(config, f)
PYCONFIG

"$omni_check_dir/omnicli" whoami --access-config "$omni_check_dir/access.json"
"$omni_check_dir/omnicli" groups --access-config "$omni_check_dir/access.json"
```

Expect `{"principal":"<your username>"}` and `{"groups":["demo"]}`. Ordinary CLI commands
remain unchanged when `--access-config` is absent; these introspection commands require an
authenticated session. Remove the temporary files afterwards with `rm -r "$omni_check_dir"`.

### Bearer configuration

Configuration format:

```json
{
  "issuer": "example-local",
  "mechanism": "bearer",
  "tokens": [{"subject": "alice", "sha256": "<64 hexadecimal digest characters>"}],
  "directory": {
    "groups": ["readers"],
    "principals": [{
      "id": "alice",
      "identities": [{"issuer": "example-local", "subject": "alice"}],
      "groups": ["readers"],
      "attributes": {"team": "operations"}
    }]
  },
  "rules": [
    {"subject": "group:readers", "resource": "command/omnicli/whoami", "action": "execute"},
    {"subject": "group:readers", "resource": "command/omnicli/groups", "action": "execute"},
    {"subject": "group:readers", "resource": "command/omnicli/doc-select", "action": "execute"},
    {"subject": "group:readers", "resource": "operation/demo/**", "action": "execute"},
    {"subject": "group:readers", "resource": "credential/demo", "action": "use"}
  ]
}
```

Use narrower operation rules for production. `mechanism: "os"` uses the effective UID as the
subject and requires no tokens or token-env flag. JWT and mTLS are reusable library authenticators;
the initial CLI configuration exposes bearer and local OS authentication only.

Unknown configuration fields, duplicate identities/principals/groups, undeclared memberships,
missing credentials, and failed directory lookups are initialization errors. Configuration and policy
are read anew on each CLI invocation. There is no directory/policy cache or background reload.

## StackQL adapter

The companion StackQL change adds `internal/stackql/accessmanagement.NewDirectory`, returning the
same `principal.Directory` against its configured SQL engine, and an explicit `InitializeSchema`
administration function. It stores issuer-qualified identity bindings, principals, attributes,
groups and membership in host-owned tables. Resolution uses one parameterized query and returns a
fresh immutable snapshot. Omni imports no database driver or SQL implementation for this feature.

This adapter does not by itself enable StackQL authentication. StackQL's transport-specific caller
configuration, session propagation, policy administration and authorization of its non-Omni
operations still need wiring before advertising protected StackQL entry points. Enabling the SDK
session on only one execution path would not protect the application as a whole.

## Black-box verification

`test/robot/access.robot` runs `whoami` and `groups` through the compiled CLI with bearer and
local OS authentication, and checks empty memberships and fail-closed authentication, directory
and policy decisions. It is included in the standard `test/robot/run.sh` suite. See
[Robot instructions](../../test/robot/robot.md#caller-access-management) for the focused command.
