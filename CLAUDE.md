# omnisdk

Before changing code, read **[docs/developer/invariants.md](docs/developer/invariants.md)**. It is
the canonical list of repository invariants — API shape, layering, durability, confidentiality — and
several of them are enforced by tests in `internal/arch/`.

The ones broken most often:

- Interfaces, not public structs. Exported structs are for serde DTOs only.
- Scope (account, region, collection, state directory) is required explicit input. Never defaulted,
  never inferred.
- `cmd/omnicli` imports nothing from `internal/`; `facade` imports only the standard library.
- Never guess about a destructive action. Unknown means report, not assume.

`.claude/local/` is confidential: never commit, publish, or transmit it.

## Build and test

```bash
go build ./... && go vet ./... && go test -race ./...
```

Manual testing and the CLI surface: [docs/developer/developer_guide.md](docs/developer/developer_guide.md),
[docs/developer/use_cases.md](docs/developer/use_cases.md).
