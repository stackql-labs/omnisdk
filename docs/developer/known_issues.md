# Known issues

## Google OAuth scopes are hardcoded, not read from the document

The token exchange asks for `https://www.googleapis.com/auth/cloud-platform`, a constant in
`internal/system_g/exchange/docx`. Nothing reads what the document declares.

It was `cloud-platform.read-only`, which reads most Google APIs and which **Compute rejects
outright**:

```
status 403: "Request had insufficient authentication scopes"
  reason: ACCESS_TOKEN_SCOPE_INSUFFICIENT
  method: compute.v1.NetworksService.List
```

That failure is about the *token*, not the identity: a principal with org-wide viewer still gets it,
because the scope caps what the token can reach before any role is consulted. It reads as a
permissions problem and is not one, which is what makes it expensive.

The documents do state scopes, in two places that disagree about granularity:

```yaml
# services/compute.yaml — the scheme, per service
securitySchemes:
  Oauth2:
    flows:
      implicit:
        scopes:
          https://www.googleapis.com/auth/compute.readonly: View your Google Compute Engine resources
          https://www.googleapis.com/auth/devstorage.read_only: View your data in Google Cloud Storage

# the same document — per operation, on compute.networks.list
security:
  - Oauth2: [https://www.googleapis.com/auth/cloud-platform]
  - Oauth2: [https://www.googleapis.com/auth/compute]
```

Neither is carried through the parse boundary: `aot.Security` exposes `Scheme()` and `Name()` and no
scopes. So the choice today is a constant, and the broad one is the only one that works everywhere.

**Consequence.** A run requests more than it needs. The identity's roles remain the real limit — a
viewer cannot write — but the token is wider than the call, which is the wrong default for
least privilege.

**Fix.** Carry the operation's declared scopes through `aot.Security`, request those, and fall back
to the scheme's when an operation names none. The per-operation declaration is the right source: it
is stated per call, which is the granularity a token should match.

## Related: `schema_driven_xml` was named but not implemented

Fixed, and worth recording because the failure mode is the same shape as the scope one — silence
rather than an error. AWS documents name `schema_driven_xml_v0.1.0` with no program body and declare
a row path of `$.line_items`, a shape only that transform produces. With the transform missing, the
path matched nothing and every query returned zero rows and said nothing about why.

Implemented in `pkg/docparse/dsl/schemaxml`. `docsem.Outliers` reports documents whose responses are
described too thinly to read, so the next instance is visible rather than silent.

## Resolved: Azure `oauth2` in the document path

Was a wiring gap rather than a missing capability: `client_credentials` auth already worked in
hand-authored plans, and `docx` simply did not map a document's `oauth2` declaration onto it. A
document declaring it now compiles to a token exchange plus the call, joined by a β edge carrying the
bearer — the same shape `service_account` takes, differing only in the grant.
