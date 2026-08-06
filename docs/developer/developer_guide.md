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
./build/omnicli resources                          # list resources (things)
./build/omnicli resources --filter storage         # regex-filter by path
./build/omnicli resources google.storage.buckets   # a resource's canonical schema
./build/omnicli methods google.storage.buckets       # a resource's methods + signatures
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

> gRPC transport (dynamic, proto-only serde over an embedded minimal `google.storage.v2` proto) is NOT
> a separate verb — it is a transport choice behind `google.storage.buckets.list`, selected by the
> optional `grpc_target` param (see the Generic DTO command section). The row shape is identical to
> REST; the protocol shows only in the traffic log. There is no bespoke grpc CLI command — the CLI is a
> pure `pkg/omnisdk` consumer.

Scope is single-account by default: AWS = the creds' account, GCP = the required `--project`, Azure = every subscription the SP can read. `blob-audit-shallow-gcp-org` audits a specific org (`--gcp-org`, required) — its direct-child projects for now; folder-nested projects await the recursive folder descent.

Tuning (any subcommand): `--parallelism` (fan-out concurrency), `--max-per-host`, `--retry-tries`, `--retry-rate`, `--limit` (stop cleanly after N output records, 0 = unlimited; a GLOBAL cap even across the multi-provider audit's disjoint DAGs).
Credentials resolve direct flag → env var → file; env vars are never required. Scope (e.g. `--project`) is **required and never inferred** — no env or key-embedded fallback.


## Generic DTO command

`run <method-path> '<args-json>'` — the JSON deserializes straight into `omnisdk.Args` via Go's intrinsic `encoding/json` (`{"params":{…},"auth":{…},"endpoint":"…","tuning":{…}}`, field names case-insensitive). `--out`/`--log` and any tuning flags still apply; the JSON may also carry `endpoint`/`tuning`. Discover a method's params first with `./build/omnicli method <path>`.

**Auth** flows entirely from the `auth` object and defaults to the canonical env vars, so it's optional. Each credential resolves **inline value → named env var → file**, and each env-var/file name defaults to the provider's canonical variable — so omit `auth` to use the standard environment, or inject secrets per-request inline: AWS `{"access_key_id":"…","secret_access_key":"…","session_token":"…"}`, GCP `{"credentials":"<SA JSON>"}` or `{"credentialsfilepath":"/path/key.json"}`, Azure `{"tenant":"…","client_id":"…","client_secret":"…"}` (or `{"type":"bearer","credentials":"<token>"}`). To point at a non-canonical env var instead of a value, use the `*_env_var` fields (e.g. `"access_key_id_env_var":"MY_AWS_KEY"`).

```bash
# 1) All buckets across the WHOLE GCP org (every project under the org) — REST.
_now="$(date +%s)" && ./build/omnicli run google.storage.buckets.list \
  '{"params":{"org":"'"${_GOOGLE_ORG_ID}"'"}}' \
  --out "./cicd/out/dto-buckets-org-${_now}.jsonl" --log "./cicd/out/dto-buckets-org-${_now}.log"

# 2) Same whole-org audit over the gRPC Storage API (proto-only serde). Transport is chosen by config
#    (grpc_target); rows are IDENTICAL to (1). Same SA key (GOOGLE_APPLICATION_CREDENTIALS) → in-process OAuth.
_now="$(date +%s)" && ./build/omnicli run google.storage.buckets.list \
  '{"params":{"org":"'"${_GOOGLE_ORG_ID}"'","grpc_target":"storage.googleapis.com:443"}}' \
  --out "./cicd/out/dto-buckets-org-grpc-${_now}.jsonl" --log "./cicd/out/dto-buckets-org-grpc-${_now}.log"

# 3) A single project — REST, then the same project over gRPC.
_now="$(date +%s)" && ./build/omnicli run google.storage.buckets.list \
  '{"params":{"project":"'"${_GOOGLE_PROJECT_ID}"'"}}' --out "./cicd/out/dto-buckets-proj-${_now}.jsonl"

_now="$(date +%s)" && ./build/omnicli run google.storage.buckets.list \
  '{"params":{"project":"'"${_GOOGLE_PROJECT_ID}"'","grpc_target":"storage.googleapis.com:443"}}' \
  --out "./cicd/out/dto-buckets-proj-grpc-${_now}.jsonl"

# 4) AWS S3 buckets, with run tuning (--limit) supplied IN the same JSON object.
_now="$(date +%s)" && ./build/omnicli run aws.s3.buckets.list \
  '{"params":{"region":"'"${_AWS_REGION}"'"},"tuning":{"Limit":25}}' --out "./cicd/out/dto-aws-${_now}.jsonl"

# 5) Azure storage accounts with config-driven auth carried IN the DTO (no --auth flag).
_now="$(date +%s)" && ./build/omnicli run azure.storage.accounts.list \
  '{"auth":{"type":"client_credentials","token_url":"https://login.microsoftonline.com/'"${AZURE_TENANT_ID}"'/oauth2/v2.0/token","client_id_env_var":"AZURE_CLIENT_ID","client_secret_env_var":"AZURE_CLIENT_SECRET","scopes":["https://management.azure.com/.default"]}}' \
  --out "./cicd/out/dto-azure-cc-${_now}.jsonl"
```