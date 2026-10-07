# Known issues

## Resolved: `schema_driven_xml` was named but not implemented

Worth recording for its failure mode — silence rather than an error. AWS documents name `schema_driven_xml_v0.1.0` with no program body and declare
a row path of `$.line_items`, a shape only that transform produces. With the transform missing, the
path matched nothing and every query returned zero rows and said nothing about why.

Implemented in `pkg/docparse/dsl/schemaxml`. `docsem.Outliers` reports documents whose responses are
described too thinly to read, so the next instance is visible rather than silent.

## Resolved: Azure `oauth2` in the document path

Was a wiring gap rather than a missing capability: `client_credentials` auth already worked in
hand-authored plans, and `docx` simply did not map a document's `oauth2` declaration onto it. A
document declaring it now compiles to a token exchange plus the call, joined by a β edge carrying the
bearer — the same shape `service_account` takes, differing only in the grant.
