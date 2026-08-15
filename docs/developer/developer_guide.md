# Manual testing

You will need a file `cicd/vol/vendor-secrets/secrets.sh` of the form

```bash

export AWS_ACCESS_KEY_ID='*****'
export AWS_SECRET_ACCESS_KEY='****'
export AZURE_TENANT_ID='****'
export AZURE_CLIENT_ID='***'
export AZURE_CLIENT_SECRET='****'
export STACKQL_GITHUB_TOKEN='****'
export STACKQL_GITHUB_USERNAME='****'
export GOOGLE_CREDENTIALS='*****'
export GOOGLE_APPLICATION_CREDENTIALS='/path/to/your/key.json' # key file must be present at this path

## Not used in auth (scope — passed via explicit flags, never read from env)
export _GOOGLE_ORG_ID='<your numerical org id>'
export _GOOGLE_PROJECT_ID='<your project short name>'
export _AWS_REGION='us-east-1'

```

```bash
go build -o build/omnicli ./cmd/omnicli

source cicd/vol/vendor-secrets/secrets.sh # populate accordingly

# Discover the facade catalog (dot-path grammar, e.g. google.storage.buckets.list):
./build/omnicli resources -q                       # JUST the names, one per line (pipeable)
./build/omnicli resources                          # list resources (things)
./build/omnicli resources --filter storage         # regex-filter by path
./build/omnicli resources omni.storage.buckets   # a resource's canonical schema
./build/omnicli methods omni.storage.buckets -q    # just this resource's method names
./build/omnicli methods omni.storage.buckets       # a resource's methods + signatures
./build/omnicli method google.storage.buckets.list   # one method's signature

_now="$(date +%s)" && ./build/omnicli encryption --aws-region "${_AWS_REGION}" --out "./cicd/out/bucket-encryption-${_now}.jsonl" --log "./cicd/out/bucket-encryption-${_now}.log"

# provision: --vpc-cidr and --subnet-cidr are required (the plan rejects instantly if either is absent)
_now="$(date +%s)" && ./build/omnicli provision --aws-region "${_AWS_REGION}" --vpc-cidr 10.0.0.0/16 --subnet-cidr 10.0.1.0/24 --out "./cicd/out/provision-${_now}.jsonl" --log "./cicd/out/provision-${_now}.log"

# GCP: service-account key needs roles/compute.networkAdmin; --project is required
_now="$(date +%s)" &&
  ./build/omnicli gcp-provision --project "${_GOOGLE_PROJECT_ID}" --region us-central1 \
    --out "./cicd/out/gcp-${_now}.jsonl" --log "./cicd/out/gcp-${_now}.log"

_now="$(date +%s)" && ./build/omnicli azure-vnets --out "./cicd/out/azure-${_now}.jsonl" --log "./cicd/out/azure-${_now}.log"

# Blob-store encryption audit across all three (AWS_*/AZURE_*/GOOGLE_CREDENTIALS from the secrets file).
_now="$(date +%s)" && ./build/omnicli blob-audit-shallow --aws-region "${_AWS_REGION}" --project "${_GOOGLE_PROJECT_ID}" --out "./cicd/out/blob-${_now}.jsonl" --log "./cicd/out/blob-${_now}.log"

_now="$(date +%s)" && time ./build/omnicli blob-audit-shallow-org --aws-region "${_AWS_REGION}" --gcp-org "${_GOOGLE_ORG_ID}" --out "./cicd/out/blob-org-${_now}.jsonl" --log "./cicd/out/blob-org-${_now}.log"

# ...or one provider at a time:
_now="$(date +%s)" && ./build/omnicli blob-audit-shallow-aws    --aws-region "${_AWS_REGION}"    --out "./cicd/out/blob-aws-${_now}.jsonl"
_now="$(date +%s)" && ./build/omnicli blob-audit-shallow-azure  --out "./cicd/out/blob-azure-${_now}.jsonl"

# Config-driven auth (switch method by JSON): client-credentials (Azure/Entra) or a pre-obtained OIDC bearer.
_now="$(date +%s)" && ./build/omnicli blob-audit-shallow-azure-auth --out "./cicd/out/blob-azure-cc-${_now}.jsonl" \
  --auth '{"type":"client_credentials","token_url":"https://login.microsoftonline.com/'"${AZURE_TENANT_ID}"'/oauth2/v2.0/token","client_id_env_var":"AZURE_CLIENT_ID","client_secret_env_var":"AZURE_CLIENT_SECRET","scopes":["https://management.azure.com/.default"]}'

_now="$(date +%s)" && ./build/omnicli blob-audit-shallow-azure-auth --out "./cicd/out/blob-azure-oidc-${_now}.jsonl" \
  --auth '{"type":"bearer","credentialsenvvar":"AZURE_OIDC_TOKEN"}'
_now="$(date +%s)" && ./build/omnicli blob-audit-shallow-gcp --project "${_GOOGLE_PROJECT_ID}" --out "./cicd/out/blob-gcp-${_now}.jsonl"

_now="$(date +%s)" && ./build/omnicli blob-audit-shallow-gcp-org --gcp-org "${_GOOGLE_ORG_ID}" --out "./cicd/out/blob-gcp-org-${_now}.jsonl" --log "./cicd/out/blob-gcp-org-${_now}.log"

# Combined, one shot: AWS + Azure + GCP merged into ONE cursor (three disjoint DAGs under a single
# output node — the consumer opens one Plan). --limit caps the union globally.
_now="$(date +%s)" && ./build/omnicli blob-audit-shallow --aws-region us-east-1 --project "${_GOOGLE_PROJECT_ID}" --out "./cicd/out/blob-all-${_now}.jsonl"
```

> The same cross-cloud audit is also a **single catalog method** — `omni.storage.buckets.list` on the
> `omni.storage.buckets` resource — so a consumer selects one path instead of assembling the member
> list itself (`./build/omnicli methods omni.storage.buckets`). It is a *composite*: its plan is the
> forest of `aws.s3.buckets.list` + `azure.storage.containers.list` + `google.storage.buckets.list`
> merged into one cursor, defined by reference to those legs so it cannot drift from them. Rows,
> schema, and the global `--limit` are identical to `blob-audit-shallow`. Run it with the generic
> `run` command (case 6 below) — the CLI deliberately gains no new verb.

> gRPC transport (dynamic, proto-only serde over an embedded minimal `google.storage.v2` proto) is NOT
> a separate verb — it is a transport choice behind `google.storage.buckets.list`, selected by the
> optional `grpc_target` param (see the Generic DTO command section). The row shape is identical to
> REST; the protocol shows only in the traffic log. There is no bespoke grpc CLI command — the CLI is a
> pure `pkg/omnisdk` consumer.

Scope is single-account by default: AWS = the creds' account, GCP = the required `--project`, Azure = every subscription the SP can read. `blob-audit-shallow-gcp-org` audits a specific org (`--gcp-org`, required) — its direct-child projects for now; folder-nested projects await the recursive folder descent.

**Grain.** Every row in the blob audit is one *bucket*. Azure's bucket analogue is a blob
**container**, not a storage account — the hierarchy is subscription → account → blob service →
container — so `azure.storage.containers.list` descends to containers and inherits the account's
encryption, HTTPS and diagnostic-logging settings. Container names are unique only within an account,
so `name` is qualified `<account>/<container>`; `public` is the container's own `publicAccess` where
it sets one, otherwise the account's `allowBlobPublicAccess`. An account with no containers still
yields a row, with `name` null.

**Access logging** is in the uniform blob schema as `access_logging` (boolean) + `access_log_target`
(where the logs land, null when off). All three clouds express it as a destination, so presence of a
target is the answer — but they surface it very differently:

| cloud | source | cost |
|---|---|---|
| AWS S3 | `GET /<bucket>?logging` → `BucketLoggingStatus.LoggingEnabled.TargetBucket`; 200 with an empty body means off | one extra call per bucket |
| GCS | `logging.logBucket` on the bucket resource, already in `buckets.list` | free |
| Azure | no such field on the storage account — the analogue is Azure Monitor **diagnostic settings** on the blob service, same ARM host and bearer | one extra call per account |

A method's **response schema carries its own input params as columns**, so signature and result contract read as one: a required input is a plain-typed column, an optional one is nullable and `null` when not supplied, and each is marked `"x-omnisdk-input": true` to separate it from provider data. Rows carry those values, so every row states the scope that produced it — for the cross-cloud composite that is the only way to attribute a row, since the legs are merged into one cursor. The columns are derived from `Params` at read time, so they cannot drift from the signature; a provider column of the same name always wins. A method that declares no params (e.g. `azure.storage.containers.list`) is untouched.

Tuning (any subcommand): `--parallelism` (fan-out concurrency), `--max-per-host`, `--retry-tries`, `--retry-rate`, `--limit` (stop cleanly after N output records, 0 = unlimited; a GLOBAL cap even across the multi-provider audit's disjoint DAGs).

> `--limit` is a budget, **not a sample**: it has no fairness across legs, so any value below the full result set biases toward whichever provider emits first. On a real org-wide run, `"tuning":{"Limit":25}` returned 24 GCP + 1 Azure rows and **zero** AWS — the AWS leg fans out three detail calls per bucket before its first row, and the budget was gone. To bound a multi-cloud look, run the legs separately with their own limits.
Credentials resolve direct flag → env var → file; env vars are never required. Scope (e.g. `--project`) is **required and never inferred** — no env or key-embedded fallback.


## Endpoints (mocking)

Every provider host is registered once, at exchange init, with `{vars}` expanded per request — so
retargeting a run is config, not a code path. The eight services:

```
aws.s3  aws.ec2  azure.login  azure.mgmt  gcp.oauth  gcp.storage  gcp.crm  gcp.compute
```

`endpoint` takes a URL string or a per-service object. Omitted services stay REAL, so you can mock one
provider and genuinely call another. Examples use the composite, which reaches every service at once;
`$M` is the mock `test/robot/run.sh` starts.

```bash
source cicd/vol/vendor-secrets/secrets.sh
M=http://127.0.0.1:8085

# whole run at the mock: scheme/host/port replaced, each service KEEPS its registered path
# (gcp.storage → $M/storage/v1, gcp.crm → $M/v3)
./build/omnicli run omni.storage.buckets.list \
  '{"params":{"region":"us-east-1","google_org":"'"${_GOOGLE_ORG_ID}"'"}}' --endpoint "$M"

# FRAGMENT override, per service — only the named parts change. Naming ONE service mocks only that
# one; anything you leave out really does call the cloud, so scope the method to match.
./build/omnicli run aws.s3.buckets.list \
  '{"params":{"region":"us-east-1"},
    "endpoint":{"aws.s3":{"scheme":"http","host":"127.0.0.1","port":"8085"}}}'

# WHOLE-url override — the full URL outright, for a mock whose layout differs from the provider's
./build/omnicli run google.storage.buckets.list \
  '{"params":{"google_org":"123456789"},
    "endpoint":{"gcp.storage":"'"$M"'/storage/v1","gcp.oauth":"'"$M"'/token","gcp.crm":"'"$M"'/v3"}}'

# one fragment at a time
#   {"gcp.crm":{"port":"7000"}}  → https://cloudresourcemanager.googleapis.com:7000/v3
#   {"gcp.crm":{"path":"/v4"}}   → https://cloudresourcemanager.googleapis.com/v4
```

A typo fails up front rather than silently leaving the run on the real cloud:

```
omnisdk: endpoint: unknown service "aws.s4" (known: aws.ec2, aws.s3, azure.login, ...)
```

Auth exchanges are services too — `azure.login` and `gcp.oauth` are in the list — so a mocked run
performs a real token exchange against the mock and carries the resulting bearer downstream. Nothing
is stubbed out; the mock rejects an unauthenticated call exactly as the provider does.

**For a consumer (e.g. stackql), this is a field, not a flag.** The CLI only passes it through:

```go
omnisdk.New("omni.storage.buckets.list", omnisdk.Args{
    Params:   map[string]string{"region": "us-east-1", "google_org": orgID},
    Endpoint: mockBaseURL, // or the per-service JSON object, as a string
})
```

So a consumer's own integration tests can point the whole cross-cloud audit at their mock without the
facade, the catalog, or the plan knowing a test is running.

`Endpoint Overrides Retarget Every Service` runs the composite through all three forms and asserts
each against captured collateral (`test/mock/expected/omni-blob-org.jsonl`) — the release gate for
mockability.

## Document-driven (no catalog entry)

`doc-select <doc.yaml> <resource>` runs a resource's SELECT straight from a stackql provider
document. Nothing is hand-authored: the verb, path, form body, response program and item path all come
out of the document, and the auth scheme it declares (`security: [hmac]` →
`x-amazon-apigateway-authtype: awsSigv4`) is applied **implicitly** — SigV4 region from `--aws-region`,
service from the document's own host.

```bash
source cicd/vol/vendor-secrets/secrets.sh

_now="$(date +%s)" && ./build/omnicli doc-select \
  pkg/docparse/stackqldoc/testdata/ec2.yaml instances \
  --aws-region "ap-southeast-2" \
  --out "./cicd/out/doc-instances-${_now}.jsonl" --log "./cicd/out/doc-instances-${_now}.log"
```

Credentials are never optional when the document declares signing — an unsigned request is not a
fallback:

```
omnicli: docx: exchange "instances" declares aws.sigv4 ("hmac") but no credentials were supplied
```

A SELECT over an empty set is zero rows, not a row of bare inputs. `--endpoint` retargets the
document's server (keeping each operation's path) so a document runs against a mock unedited.

Known gap: **one page only.** The document reads its next-page token from `$.next_page_token` — the
*transformed* body — while the HTTP layer's continuation reads the decoded wire response. Bridging
those is the next piece.

## Generic DTO command

`run <method-path> '<args-json>'` — the JSON deserializes straight into `omnisdk.Args` via Go's intrinsic `encoding/json` (`{"params":{…},"auth":{…},"endpoint":"…","tuning":{…}}`, field names case-insensitive). `--out`/`--log` and any tuning flags still apply; the JSON may also carry `endpoint`/`tuning`. Discover a method's params first with `./build/omnicli method <path>`.

**Auth** flows entirely from the `auth` object and defaults to the canonical env vars, so it's optional. Each credential resolves **inline value → named env var → file**, and each env-var/file name defaults to the provider's canonical variable — so omit `auth` to use the standard environment, or inject secrets per-request inline: AWS `{"access_key_id":"…","secret_access_key":"…","session_token":"…"}`, GCP `{"credentials":"<SA JSON>"}` or `{"credentialsfilepath":"/path/key.json"}`, Azure `{"tenant":"…","client_id":"…","client_secret":"…"}` (or `{"type":"bearer","credentials":"<token>"}`). To point at a non-canonical env var instead of a value, use the `*_env_var` fields (e.g. `"access_key_id_env_var":"MY_AWS_KEY"`).

```bash
# 1) All buckets across the WHOLE GCP org (every project under the org) — REST.
_now="$(date +%s)" && ./build/omnicli run google.storage.buckets.list \
  '{"params":{"google_org":"'"${_GOOGLE_ORG_ID}"'"}}' \
  --out "./cicd/out/dto-buckets-org-${_now}.jsonl" --log "./cicd/out/dto-buckets-org-${_now}.log"

# 2) Same whole-org audit over the gRPC Storage API (proto-only serde). Transport is chosen by config
#    (grpc_target); rows are IDENTICAL to (1). Same SA key (GOOGLE_APPLICATION_CREDENTIALS) → in-process OAuth.
_now="$(date +%s)" && ./build/omnicli run google.storage.buckets.list \
  '{"params":{"google_org":"'"${_GOOGLE_ORG_ID}"'","grpc_target":"storage.googleapis.com:443"}}' \
  --out "./cicd/out/dto-buckets-org-grpc-${_now}.jsonl" --log "./cicd/out/dto-buckets-org-grpc-${_now}.log"

# 3) A single project — REST, then the same project over gRPC.
_now="$(date +%s)" && ./build/omnicli run google.storage.buckets.list \
  '{"params":{"google_project":"'"${_GOOGLE_PROJECT_ID}"'"}}' --out "./cicd/out/dto-buckets-proj-${_now}.jsonl"

_now="$(date +%s)" && ./build/omnicli run google.storage.buckets.list \
  '{"params":{"google_project":"'"${_GOOGLE_PROJECT_ID}"'","grpc_target":"storage.googleapis.com:443"}}' \
  --out "./cicd/out/dto-buckets-proj-grpc-${_now}.jsonl"

# 4) AWS S3 buckets, with run tuning (--limit) supplied IN the same JSON object.
_now="$(date +%s)" && ./build/omnicli run aws.s3.buckets.list \
  '{"params":{"region":"'"${_AWS_REGION}"'"},"tuning":{"Limit":25}}' --out "./cicd/out/dto-aws-${_now}.jsonl" \
  --log "./cicd/out/dto-aws-${_now}.log"

# 5) Azure storage accounts with config-driven auth carried IN the DTO (no --auth flag).
_now="$(date +%s)" && ./build/omnicli run azure.storage.containers.list \
  '{"auth":{"type":"client_credentials","token_url":"https://login.microsoftonline.com/'"${AZURE_TENANT_ID}"'/oauth2/v2.0/token","client_id_env_var":"AZURE_CLIENT_ID","client_secret_env_var":"AZURE_CLIENT_SECRET","scopes":["https://management.azure.com/.default"]}}' \
  --out "./cicd/out/dto-azure-cc-${_now}.jsonl"

# 6) The CROSS-CLOUD COMPOSITE as one method: AWS + Azure + GCP in a single select. Params are the
#    union of the legs' scope inputs (region required; exactly one of google_project/google_org for
#    the GCP leg; Azure needs none — its scope is the SP's reach). Same rows as blob-audit-shallow.
_now="$(date +%s)" && ./build/omnicli run omni.storage.buckets.list \
  '{"params":{"region":"'"${_AWS_REGION}"'","google_project":"'"${_GOOGLE_PROJECT_ID}"'"}}' \
  --out "./cicd/out/dto-omni-${_now}.jsonl" --log "./cicd/out/dto-omni-${_now}.log"

# ...or org-wide for the GCP leg.
_now="$(date +%s)" && ./build/omnicli run omni.storage.buckets.list \
  '{"params":{"region":"'"${_AWS_REGION}"'","google_org":"'"${_GOOGLE_ORG_ID}"'"}}' \
  --out "./cicd/out/dto-omni-org-${_now}.jsonl"

# 
_now="$(date +%s)" && ./build/omnicli run omni.storage.buckets.list \
  '{"params":{"region":"'"${_AWS_REGION}"'","org":"'"${_GOOGLE_ORG_ID}"'"}}' \
  --out "./cicd/out/dto-omni-org-${_now}.jsonl"
```