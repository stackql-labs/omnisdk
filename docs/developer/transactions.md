# Provider SQL transactions

Omni owns transaction state, scheduling and recovery. stackql translates SQL and holds
an opaque session-bound capability. The transaction ID is only a recovery locator.

- Journal: ordered effects, inverse addressing and recovery attestations.
- Ledger: resource intent/identity/version plus `.transactions/<id>` lifecycle checkpoints.
- Leases: the same collection/resource namespace as IaC; held through commit or undo.
- Authentication: required. Every operation is authorized; another session cannot use a
  handle. Resume checks the owner, reacquires scope, validates resource versions and
  issues a fresh capability. Queued work is not permission to replay an uncertain call.

`TransactionSession.BeginWithRegistry/Execute/Queue/Drain/Commit/Rollback/Resume/Status/Recover`
compose the existing managed mutation machinery. `Plan` accepts host-translated graphs.
No desired-state comparison is performed. Auth DTOs are not stored in checkpoints.

Known scope is locked at BEGIN. `dynamic_scope: true` permits atomic expansion without
releasing earlier claims; conflicts fail rather than wait in a deadlock. Execution handles
expire after `timeout_seconds` (default 900); detached transactions require explicit resume.
Leases use `lease_seconds` (default 300). Resume waits for prior claims to expire.
`BeginWithRegistry` checks declared inverses and permissions and pins document fingerprints.

## stackql usage

Opt in before BEGIN; `state` is an optional name under the existing IaC state root,
including its authenticated principal/tenant partition. Keys must match other users
of the same objects and coordination store.

```sql
SET "$.transaction" = '{"collection":"demo","mode":"lazy","keys":["item"],"targets":{"stackql_unstable_demo.service.items":{"key":"item","identity":"id","addressed_by":"id"}}}';
BEGIN;
-- INSERT / SELECT ...
SELECT * FROM stackql_preview.txn.status;
COMMIT; -- or ROLLBACK
```

Without `mode`, existing `--session` rollback configuration maps `nop` to lazy and
`eager` to eager. Both modes compensate executed work. Reads, RETURNING and COMMIT
flush predecessors; ROLLBACK discards unexecuted work. Consume read cursors before
ending the transaction. To resume, add `resume_id` to the configuration and BEGIN.

Uncertain work permits only inspection/recovery. After verifying provider outcomes,
use `stackql_preview.txn.recover` with the transaction `version` read in the resumed session, JSON
`resources` (`key`, `outcome`, resource `version`, and identity for `applied`) and an
explicit `continue` decision. Recovery itself makes no provider request.

## Migration limits

- New execution currently accepts creates with an explicit document-declared delete
  inverse. Updates, deletes and unsupported inverse tokens fail before dispatch.
- Provider aliases are unsupported; stackql currently binds one logical target per resource address.
- Stop or isolate the previous executor before takeover: local leases cannot fence a
  provider request already in flight. Verify uncertain outcomes before recovery.
- Transaction configuration is frozen while active; nested transactions are unsupported.
- Legacy coordinators remain for sessions without `$.transaction`; they are not upgraded
  by this work. Remove them only after equivalent supported behavior is verified.
- Remote APIs do not provide database atomicity; other actors can observe effects.
  Changed versions or missing outcomes block continuation/undo rather than overwriting others.
- GC is not implemented. Active, unresolved and compensating transactions pin their
  checkpoints, resource versions and journals; terminal retention remains a separate policy.

Cross-repository validation uses a build-only modfile against local Omni. Publish the
Omni API, then pin that release in stackql before running its normal CI build.
