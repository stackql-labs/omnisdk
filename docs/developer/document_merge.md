
# Document Merge

Docuemnt merge supports behaviour specialisation and document augmentation, in isolation.  This is highly useful for developing new function, and also configurign behaviours in a context dependent manner; in the simplest case, the context being session scoped.

In simple terms, one can at any time merge document fragments into the existing provider document corpus, creating a scoped view of provider documents.  This can significantly alter behaviour.  This aspect is highly useful for configuring IAC workflows, developing provider documents, to name but two possible applications.


## Details

Patches use **RFC 7386 JSON Merge Patch** against a service document:

- Objects merge recursively; scalars and arrays replace existing values.
- `null` deletes a member.
- Patches apply in the supplied order.
- `service` identifies `<provider>.<service>` using the registry's provider name.

The base registry stays unchanged. Use the returned registry path for subsequent queries and IaC runs; callers using the base path retain the original behaviour.

## CLI

Save a patch configuration as `merge.json`. This example changes a response row selector:

```json
{
  "patches": [{
    "service": "stackql_unstable_google.compute",
    "merge": {
      "components": {
        "x-stackQL-resources": {
          "networks": {
            "methods": {
              "list": {
                "response": {"objectKey": "$.items"}
              }
            }
          }
        }
      }
    }
  }],
  "doc_cache": {"dir": "/tmp/omnisdk-doc-views"}
}
```

```bash
REGISTRY="$(build/omnicli doc-merge test/corpus/registry @merge.json)"
# Pass "$REGISTRY" as the registry argument to subsequent commands.
```

For a complete IaC example, see [AWS nginx](../../cicd/examples/aws-nginx/README.md).

## Go SDK

```go
view, err := omnisdk.EffectiveRegistry(registry, []omnisdk.DocPatch{
    {Service: "stackql_unstable_google.compute", Merge: patchJSON},
}, omnisdk.DocCache{Dir: cacheDir})
if err != nil {
    return err
}
// Pass view instead of registry to Prepare and other document operations.
```

`patchJSON` is a `json.RawMessage` containing the merge object. The returned path is the scoped view's handle; no global session switch occurs. See [use cases](use_cases.md#examples) for a lifecycle patch and query example.

## Cache and lifetime

The same base path and patches reuse a cached view. Changed patches produce a different view. Keep the base immutable: unpatched files link back to it, and base contents are not part of the cache key.

`DocCache.Fresh` (JSON `"fresh": true`) rebuilds an existing view. Do this before queries start; avoid refreshing a view while readers use it. Keep its cache directory available for the lifetime of those readers.

