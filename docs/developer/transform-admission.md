# Transform inbox admission

A consumer can declare when its inbox has enough evidence to run its transform and action.
Omitting `admission` preserves the existing ALL-dependencies scheduling. Declaring it allows a
predicate to admit a partial inbox.

The transform DTO has `type`, `body`, and an optional `admission` program with its own `type` and
`body`. Both programs can use `starlark`. An expression is evaluated directly; a multiline program
assigns its return value to `result`. Transforms return JSON objects; admission returns a boolean.

## Example

This graph wiring lets either a completed operation or a RUNNING database admit an update:

```json
{
  "to": "update",
  "inbound": [
    {"from": "create", "src": "id", "as": "id"},
    {"from": "operation", "src": "status", "as": "operation"},
    {"from": "detail", "src": "state", "as": "detail"}
  ],
  "transform": {
    "type": "starlark",
    "body": "{\"id\": inbox[\"id\"]}",
    "admission": {
      "type": "starlark",
      "body": "(edges[\"operation\"].present and edges[\"operation\"].value == \"DONE\") or (edges[\"detail\"].present and re.fullmatch(\"RUNNING|AVAILABLE\", edges[\"detail\"].value))"
    }
  },
  "provides": ["id"]
}
```

The Go equivalent is `omnisdk.NewWiringWithTransform`, taking a `dsl.Transform`.
The operation/detail queries and their input bindings are declared as ordinary graph nodes and
wirings. Admission does not invent queries or execute them through a dereference.

Run the mocked HTTP create/observe/update example:

```sh
go test ./pkg/omnisdk -run '^TestStarlarkAdmissionMockDatabase$' -v
```

That test holds the operation read open indefinitely. The detail read returns RUNNING, the update
runs once, and the losing operation read is cancelled. Lower-level scheduler tests exercise either
observation winning.

## Program inputs

`inbox` contains the contributions that have arrived. `edges` is keyed by the consumer's input name
(the wiring's `as`), with immutable snapshots:

| Field | Meaning |
|---|---|
| `present` | A contribution arrived, including an explicit null value. |
| `value` | The most recently arrived value; `None` when absent or explicitly null. |
| `done` | Its producer ended, with or without a value. |
| `error` | Producer failure text, or `None`. |

A predicate may use `done` and `error` to distinguish pending, empty, and failed observations.
Errors do not themselves imply readiness. When all alternative reads end without satisfying the
predicate, execution reports unsatisfied admission and does not invoke the consumer.

After admission, the transform sees the same snapshots. It must supply any inputs the consumer
actually needs. Absent observation values remain absent in `edges`; the ordinary binding layer may
represent missing input slots as null in `inbox`.

`re.search(pattern, text)` and `re.fullmatch(pattern, text)` return booleans. These are host-provided
helpers backed by Go's regexp engine, not Python's full `re` API.

## Execution and limits

V1 races independent, exclusively consumed read observation nodes. Graph SELECT nodes are marked
read-only by the compiler; low-level plans opt in with `plan.WithReadOnly`. Mutations and
authentication exchanges remain requisite predecessors and are never cancelled as losing
observations. Each admitted consumer runs once per upstream row. Caller cancellation continues to
apply to the consumer after the losing reads are cancelled.

Shared observation producers and observation cycles are rejected before execution. This is not yet
a general planner for alternative query subgraphs or a sequential fallback policy. Existing plans
without admission retain their existing fan-out and cycle behavior. Admission on document request
transforms is supported; response-transform admission is rejected because it is not an inbound
dependency gate.

Starlark source is validated during planning, before effects execute. V1 accepts a deliberately
bounded subset: comparisons, boolean expressions, indexing, conditionals, literals, local
assignments, `len`, `bool`, `all`, `any`, dictionary `get`, and the regexp helpers. It rejects loads,
loops, comprehensions, user functions, dynamic calls, builtin rebinding, mutation, and arithmetic
that could amplify allocations. A conservative analysis also bounds expanded composite values,
including aliases that could otherwise make native equality expensive.

Limits are 16 KiB source, 1,024 syntax nodes, 100,000 VM steps, 1 MiB inbox/edge-snapshot/output
encodings, and 64 levels of value nesting. Regexp patterns must be literals of at most 1,024 bytes;
there may be at most eight regexp calls, and each text argument is limited to 64 KiB. These limits
bound the supported subset; they are not a claim that arbitrary Starlark is statically provable safe.

## Canonical input selection

`transform.required` declares hard-required **raw inbox inputs**, independently of the admission
program. A custom admission predicate cannot waive these requirements. Null and empty string do not
satisfy them; false and zero do. Inputs produced by the transform belong in `provides`, not `required`.

A wiring can list potential `candidates` separately from its explicit `inbound` edges:

```json
{
  "to": "subnets",
  "candidates": [
    {"from": "vpcs", "src": "id", "as": "vpc_id", "canonical": true, "cost": 1},
    {"from": "lookup", "src": "vpc_id", "as": "vpc_id", "cost": 5}
  ],
  "transform": {
    "type": "starlark",
    "required": ["vpc_id"],
    "body": "{\"VpcId\": inbox[\"vpc_id\"]}"
  },
  "provides": ["VpcId"]
}
```

The producer nodes must be declared in the possible-flow graph. In Go, use
`WithInboundCandidates(wiring, ...InboundCandidate)`. Candidates are consulted only for inputs named
in `required`; they are not execution dependencies until selected.

For each required input, existing caller values or explicit inbound bindings take precedence.
Otherwise canonical candidates rank first, followed by ascending nonnegative cost. Equal scores
retain declaration order. Missing producer aliases and candidates that would introduce a dependency
cycle are skipped. An input with no applicable source fails planning before requests are sent.

Unselected candidate-only read nodes and their private upstream reads are pruned. Nodes explicitly
used by other wirings, global filters, outputs, or gates are retained. Per-node shaping and read hints
do not force an unused candidate to run. Default candidates must
be reads; mutations require explicit graph declarations. Existing explicit edges and admission
observation alternatives are not silently replaced by cheaper candidates.

Selection is greedy per input: it sorts candidates and follows the chosen dependencies, without
searching all edge subsets or backtracking over earlier choices. Costs are local estimates; there
is no global/shared-work cost optimisation. Structural applicability does not prove a remote read
will succeed. A selected candidate whose own required inputs cannot be planned produces an error,
rather than initiating an exhaustive search for a different overall plan.

## Canonical example: alternative input requirements

Google Compute `instances.bulkInsert` illustrates **alternative input requirements** (a disjunction,
or inclusive OR):

```text
project AND zone AND count AND (sourceInstanceTemplate OR instanceProperties)
```

A template alone, explicit properties alone, or both together can satisfy the method. Both means
template plus overrides; neither is insufficient. Put the common inputs in `transform.required` and
the alternative condition in `transform.admission`:

```json
{
  "required": ["project", "zone", "count"],
  "admission": {
    "type": "starlark",
    "body": "inbox.get(\"sourceInstanceTemplate\") != None or inbox.get(\"instanceProperties\") != None"
  }
}
```

This is different from canonical candidate edges: those select an **alternative supplier of the same
input**. Here, **different input sets** satisfy the method. No upstream-query search is inferred from
the opaque admission program.

The [canonical example](../../cicd/examples/alternative-input-requirements/README.md) includes the
mock-only Google-shaped registry and a Robot suite exercising all three accepted combinations,
neither alternative, and missing common requirements. Rejected inputs must cause zero mutations.

Resource access and release rules are documented in [resource locking and ledger recovery](resource-locking.md).
Admission determines when an operation may run; managed ownership and graph references determine
its exclusive target locks and shared dependency guards.
