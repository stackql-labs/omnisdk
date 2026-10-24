
# omnisdk

The implementation of `System-G`.

## Provider document verification

Verification is opt-in. Pass `stackqldoc.WithVerifier(v)` to `Open` or
`OpenRegistry`, where `v` is constructed by the shared
`github.com/stackql/stackql-provider-registry/signing/pqsig.NewVerifier`.
Its default trust store is embedded; private registries can supply an explicit
`kid` to hex ML-DSA-65 public-key map.

Provider metadata, service documents and cross-document operation references
are authenticated before parsing. Missing/invalid signatures and unknown keys
fail closed with the document path. Verified reads bypass the shared parse cache.
No legacy certificate or `.sig` fallback exists.

For facade plans, set `Args.DocumentVerifier`; for discovery and table/view
metadata, pass `stackqldoc.WithVerifier(v)` as the optional document option.
Omitting the verifier preserves existing unverified behavior. Document mutation
and patching invalidate signatures; re-sign patched trees before verified use.
This does not add remote archive download or extraction verification.
