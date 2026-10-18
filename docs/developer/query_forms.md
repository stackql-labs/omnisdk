# Query forms

Every request is a graph plan: exchanges joined by data edges and control edges, reads and writes
alike. There are two forms. Both run the same plan; they differ only in what surrounds it.

## Ad hoc

A query — written as SQL or as a graph — is bound to methods and run. Reads and writes go through
the same graph. A write is never retried, since a retry after the request reached the provider
repeats the effect. A failed write is reported by what is known: rejected (it did not happen),
outcome unknown (it may have happened; read the target before retrying), or not attempted.

Optionally, each write's intent is recorded and synced before its request is sent. Nothing is locked
and nothing is undone: two runs may write at once, and a failed run leaves what it did.

## Converging

The same plan, wrapped in four things:

| | what it is | what it is for |
|---|---|---|
| **lock** | an expiring hold on the collection for the length of the run | keeps two runs off the same resources; a dead run's hold expires rather than waiting to be released |
| **ledger** | per resource: the last intent applied and the identity the provider gave the object | read back by the run to decide, against the live object, whether each resource is absent, matches, or has drifted, and what to send |
| **WAL** | each effect the run attempts, recorded and synced before its request | intent is on disk before any effect, so a crash never leaves an effect nothing knows to ask about |
| **undo log** | the WAL, read backwards | a failed run deletes what it created, latest first, retrying what a provider refuses while a dependent exists, until a pass makes no progress. Given as desired state, it also restores what an update replaced; given as a graph, updates and deletes are reported as completed, not reversed |

Every write belongs to a declared resource, so each has an entry in the ledger: its intent is
recorded before the request and its identity after it. Credentials travel with the request and are
never recorded.

The resources may be given as a graph or as a flat list of desired state; both run on the same plan
engine, under the same lock, ledger and WAL.

## Side by side

| | ad hoc | ad hoc, recorded | converging |
|---|---|---|---|
| intent recorded before each write | — | yes | yes |
| collection locked | — | — | yes |
| state read back across runs | — | — | yes |
| failed run undone | — | — | yes |
