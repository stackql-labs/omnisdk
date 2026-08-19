
# Use cases


Broadly, the reuisite setup for excret and build here is identical to [the developer guide](/docs/developer/developer_guide.md).  We will repeat some things for easy of copy paste, but not all, refere developer guide in all cases for full clarity on build and auth.

```bash
go build -o build/omnicli ./cmd/omnicli

source cicd/vol/vendor-secrets/secrets.sh # populate according to dev guide

```

## Bucket check cross cloud

```bash

_now="$(date +%s)" && ./build/omnicli run omni.storage.buckets.list \
  '{"params":{"region":"'"${_AWS_REGION}"'","google_project":"'"${_GOOGLE_PROJECT_ID}"'"}}' \
  --out "./cicd/out/dto-omni-${_now}.jsonl" --log "./cicd/out/dto-omni-${_now}.log"

```

## Access Reviews

SOC 2 CC6.2 / CC6.3, ISO 27001 A.5.18.

```bash

_now="$(date +%s)" && ./build/omnicli run omni.iam.principals.list \
  '{"params":{"region":"'"${_AWS_REGION}"'","google_project":"'"${_GOOGLE_PROJECT_ID}"'"}}' \
  --out "./cicd/out/principals-${_now}.jsonl" --log "./cicd/out/principals-${_now}.log"

```

One row per principal, AWS IAM + Entra ID + GCP IAM bindings (`google_org` for the whole org instead
of one project):

```json
{"provider":"entra","principal_type":"user","principal":"kieran.rimmer@stackql.net","principal_id":"059cd156-0f89-4f60-ae56-662b51198866","enabled":true,"created":"2022-04-28T05:18:40Z","grant":null}
{"provider":"aws","principal_type":"user","principal":"denamo","principal_id":"AIDA376P4FQS664SD2TOY","enabled":null,"created":"2022-12-09T04:15:24Z","grant":null}
{"provider":"gcp","principal_type":"user","principal":"javen@stackql.io","principal_id":"user:javen@stackql.io","enabled":null,"created":null,"grant":"roles/owner"}
```

Or one source at a time:

```bash

./build/omnicli run aws.iam.principals.list '{"params":{"region":"'"${_AWS_REGION}"'"}}'
./build/omnicli run entra.identities.list '{}'
./build/omnicli run gcp.iam.principals.list '{"params":{"google_project":"'"${_GOOGLE_PROJECT_ID}"'"}}'
./build/omnicli run gcp.iam.principals.list '{"params":{"google_org":"'"${_GOOGLE_ORG_ID}"'"}}'

```

`enabled` is null where a provider does not report it — not false. `grant` is set where the source
states one (GCP bindings); AWS and Entra list the identity, not its grants.

Not yet included: Google Workspace directory, Azure role assignments.
