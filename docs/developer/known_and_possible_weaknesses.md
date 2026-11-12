# Known and possible weaknesses

- Concurrent document refresh: `EffectiveRegistry(..., Fresh: true)` briefly removes the existing view's path during replacement. SDK callers running queries concurrently against that path could fail. Avoid refreshing an in-use view; concurrent queries within a session have not been established.
- Retention and garbage collection: ledger versions/tombstones, journals, diagnostics and logs need a bounded retention policy. Cleanup must preserve current state, active runs, unresolved outcomes and records required for recovery or compensation.
