# Resource bindings

References work recursively in IaC `desired` objects and arrays, and as `write_once` values:

```json
{"from": "self", "attribute": "key"}
{"from": "input", "attribute": "vpc_name"}
{"from": "env", "attribute": "AWS_REGION"}
```

| Source | Values |
| --- | --- |
| `self` | `key`, `collection`, `provider`, `address`, `desired` and `write_once` of this resource |
| `input` | Caller-supplied JSON data |
| `env` | Caller-supplied environment/context data |

Attributes support dotted paths; an exact key takes precedence. `self` references may depend
on other declaration fields; cycles fail. Missing values, unknown sources and malformed
references fail during preparation, before any mutation. There is no fallback between sources
or interpolation of strings such as `${self}.key`. Live API identities are not declaration attributes.

Values retain their JSON types, including explicit nulls and large integers. `write_once` references
must resolve to strings. Returned `input`/`env` values are data, not further expressions.

The SDK takes `Args.Inputs` and `Args.Environment`; it never reads process environment variables
for these bindings. Bindings resolve to a snapshot before execution; the ledger stores resolved intent.
Sibling dependencies continue to use `inbound`.

## CLI

Files may contain a top-level `inputs` object. `iac-apply --input '{"vpc_name":"demo"}'`
or `--input @inputs.json` overrides matching keys. Precedence is CLI input, file `inputs`,
then `args.inputs`. The CLI reads only process variables explicitly referenced by `env`;
values already supplied in `args.environment` take precedence.

See [the AWS example](../../cicd/examples/aws-vpc-subnet/README.md).
Tagging is ordinary user-declared request data. Both `correlation_param` and
`debug_correlation_param` are retired and rejected; neither injects labels.

`write_once` supplies request inputs whose changes do not trigger reconciliation.
`desired` declares state to compare and update. The old resource field `params` is rejected; use `write_once`. This rename does not affect method-call `Args.params`.
