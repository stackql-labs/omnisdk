# Alternative input requirements

This is the canonical example of **alternative input requirements**: a method can accept different
sets of inputs. Logically, the requirement is a disjunction (inclusive OR), not an exclusive choice.

Google Compute's `compute.instances.bulkInsert` accepts:

| Inputs in addition to common requirements | Meaning |
| --- | --- |
| `sourceInstanceTemplate` | Create from an existing template. |
| `instanceProperties` | Supply instance properties directly. |
| Both | Use a template and override selected properties. |
| Neither | Insufficient inputs; do not send the mutation. |

The repository's Google document describes `instanceProperties` as required when
`sourceInstanceTemplate` is absent, and explicitly permits combining them. See
[BulkInsertInstanceResource](../../../test/corpus/registry/googleapis.com/v00.00.00000/services/compute.yaml#L66433).
The contents of either branch must still be valid for Google; this example isolates input admission,
not every Compute API validation rule or actual VM provisioning.

The common requirements remain compulsory:

```text
project AND zone AND count AND (sourceInstanceTemplate OR instanceProperties)
```

## Declaration

[bulk-insert.json](bulk-insert.json) uses external caller inputs: scope in node `params`, and create
arguments in `body`. No observation query is needed. Its consumer transform declares:

```json
{
  "required": ["project", "zone", "count"],
  "admission": {
    "type": "starlark",
    "body": "inbox.get(\"sourceInstanceTemplate\") != None or inbox.get(\"instanceProperties\") != None"
  }
}
```

`required` expresses unconditional requirements. The admission program expresses the alternative
requirements. Both fields may be present, so this is inclusive OR. No shaping body is necessary:
this identity transform passes admitted inputs through to the existing request builder.

This differs from **alternative suppliers**: canonical/cost-ranked candidate edges offer several ways
to obtain the same named input. Here, different names can satisfy the method. The planner does not
attempt to analyse the Starlark predicate and search for combinations of upstream queries.

## Run the mocked Robot test

From the repository root, with the existing Robot environment:

```sh
go build -o build/omnicli ./cmd/omnicli
test/mock/.venv/bin/robot --outputdir test/robot/out/alternative-inputs \
  test/robot/alternative_inputs.robot
```

`test/robot/run.sh` bootstraps that environment and discovers this suite automatically in the normal
Robot run. The mock uses Python's standard library and an ephemeral loopback port; no cloud account
or credentials are required.

The suite runs the actual CLI against the [mock-only registry](registry). It checks that each valid
combination sends exactly one POST, to the expected endpoint, with the original body unchanged.
Neither alternative, missing project, and missing count must fail with **zero mutation requests**.
The fixture disables the separate Google post-create lifecycle default so that it tests only input
requirements.

See [transform inbox admission](../../../docs/developer/transform-admission.md) for the DTO,
edge snapshots, language limits, and candidate selection.
