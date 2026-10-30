# Declared IaC update routing

Run from the repository root:

```sh
go build -o build/omnicli ./cmd/omnicli
python3 cicd/examples/update-routing/run.py \
  --binary build/omnicli \
  --out cicd/out/update-routing-001
```

Choose a fresh output directory for each run. Python 3 uses only its standard library.
The runner starts a local mock API, invokes the actual CLI, verifies the observed effects,
then destroys the managed object. No cloud account or credentials are needed.

| File | Intent |
| --- | --- |
| `01-create.json` | Create a small red widget; applying it again must be a no-op. |
| `02-update.json` | Change its color to blue through the declared update route. |
| `03-rejected-update.json` | Change its size; no route permits this, so reject before mutation. |
| `registry/` | Self-contained provider documents, operations and the `updates` declaration. |

The provider rule requires a color change and permits only color changes. The update
target references the existing method; the resource inputs contain intent, not routing logic.
Each apply uses the same collection and ledger, with a different run ID. The runner saves
JSONL results, stderr and API-call transcripts beneath the chosen output directory.
On an unexpected failure it preserves those files for diagnosis and exits unsuccessfully.

Inspect recommendations without running the API or changing anything:

```sh
build/omnicli doc-update-routes cicd/examples/update-routing/registry \
  stackql_unstable_widgets.things.widgets '["color"]'
```

`["size"]`, `["color","size"]` and `[]` return no recommendations. Discovery returns
recommendations; it does not execute or choose a plan.
