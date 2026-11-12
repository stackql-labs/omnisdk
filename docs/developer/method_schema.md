# Method schema discovery

`DescribeMethod` retains its existing `MethodSignature` interface. Document-backed signatures
also implement `SchemaMethodSignature`, exposing read-only `BodySchema()` and `RowSchema()`.
`FieldSchema` exposes logical/wire names, declared type/format, required status, descriptions,
enums, object properties and array items. References remain traversable, including recursive
references; returned slices are independent copies. No parsed YAML document is retained.

For bounded host/MCP presentation, use:

```go
fields, err := omnisdk.DescribeMethodFields(registry,
    "stackql_unstable_google.compute.instances", "insert",
    omnisdk.MethodSchemaOptions{
        Path: "data__networkInterfaces.accessConfigs",
        Depth: 2,
        Descriptions: true,
        ParamType: "input",
    })
```

Depth defaults to 1, with a maximum of 32. Deeper nodes retain their type, child count and
`truncated` flag; arrays also expose a shallow item type. Each response is limited to 4096
expanded nodes; excessive requests fail and should narrow their path or depth. Descriptions
are omitted unless requested. `param_type` accepts `input`, `output`, `input_required` or
`input_optional`. Native body names and their exposed aliases work in paths; array items
are traversed implicitly.

Optional query parameters declared on every operation are treated as document-global controls
and omitted unless `IncludeGlobal` is true. Required server/path scope remains visible.
Undeclared types remain unspecified; discovery never substitutes `string` for unknown types.

Response rows follow the declared object key after the transform. A malformed, self-referencing
XML display envelope uses the original response schema when available. If the document has
replaced that original too, output discovery reports an explicit schema error: it cannot infer
missing row fields. The pinned S3 GET lifecycle document has this defect; request-only discovery
remains available.

The stackql adapter must consume this optional signature capability or `DescribeMethodFields`
and expose depth/path controls in its MCP tool. Updating Omni alone does not update that adapter.
