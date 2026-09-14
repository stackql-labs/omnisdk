# Repository invariants

Rules that hold across the codebase. They are not style preferences: each one exists because
breaking it has already cost something. Where a rule is machine-checkable it is checked, and the
test is named.

## API shape

**1. Interfaces, not public structs.** Exported concrete structs are not the API. Keep concrete
types unexported and return interfaces from constructors. The one exception is a **serde DTO**: a
value that exists to be marshalled or handed across the boundary as data, with no behaviour —
`Args`, `Auth`, `Param`, `Tuning`. If it has methods, or callers might want a second implementation,
it is not a DTO.

*Not* "use a struct now and add an interface once there are two implementations". Interface first.

**2. A feature is designed behind an interface before it is built.** Non-negotiable. A new
capability starts as a contract in `facade`, then an implementation.

**3. Scope is explicit input, never inferred.** Which account, which region, which collection, which
state directory — anything that decides *what a run acts on* is required from the caller. No
defaults, no environment fallbacks, no derivation from another flag. A wrong guess here applies real
changes to the wrong resources.

Auth credentials are not scope and may come from the environment.

## Layering

**4. `internal/system_g/facade` is the contract root and imports the standard library only.**
Enforced: `TestFacadeDependsOnStdlibOnly`.

**5. Durable-machinery packages see contracts only.** `ledger`, `merge`, `journal`, `lease` and
`semantics` depend on `facade` and nothing else in the module: the executor calls them, never the
reverse. Enforced: `TestLeafImplementationsSeeContractsOnly`.

**6. `pkg/omnisdk` is the public facade.** It owns its own DTOs; `system_g` stays internal.
`cmd/omnicli` imports nothing from `internal/` — it consumes the facade exactly as an external
caller would, which is what keeps the facade honest.

## Durability and effects

**7. Intent is recorded before the effect, never after.** Both the per-key ledger entry and the run
journal are written and fsynced before a wire call. A record that describes an effect that was never
attempted is worse than useless — it makes a later run compensate something it did not cause — so
the record goes in only once the call is certain to be made.

**8. Never guess about a destructive action.** If the target cannot be read, "did this land?" is
unknown and must be reported, not assumed. If a step updated an object rather than creating it, its
compensation restores the prior intent — it does not delete. Both of these were shipped wrong once
and destroyed, or would have destroyed, real resources.

**9. Persist resolved intent, never API responses.** Actual state is read live. Only identity is
kept from a response, and identity means everything the inverse call needs to address the object.

**10. Stamp a correlation key on the target at create** wherever the API allows a writable,
searchable field, so the store is a cache rather than the sole link to reality. Losing the ledger
must not orphan resources.

## Confidentiality

**11. `.claude/local/` is confidential.** Never commit it, publish it, or send its contents to any
external service.

## Concurrency

**12. Concurrency is built in from day one**, not retrofitted. In-process latches are ordered and
never held across a wire call; cross-run exclusion is a durable, expiring lease.
