
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


## IAC

Converges a VPC and a subnet in it, recording intent in a durable ledger before each call. Unlike
`provision`, which issues both creates and remembers nothing, this is re-runnable: a second run
against unchanged intent issues no API calls at all.

`--name` is the collection — the ledger key prefix, the lease scope, and the correlation tag stamped
on each object. `--state` is where that ledger lives; one state directory holds many collections.
Neither is defaulted: both decide which resources a run applies to.

```bash
./build/omnicli iac-provision --aws-region us-east-1 \
  --state cicd/work/iac-state --name scratch \
  --vpc-cidr 10.42.0.0/16 --subnet-cidr 10.42.1.0/24 \
  --vpc-tags '{"Name":"scratch"}' --subnet-tags '{"Name":"scratch"}'
```

```json
{"identity":"vpc-08e599d96e6bbe5fd","key":"scratch/aws/ec2/vpc","status":"applied"}
{"identity":"subnet-08300cf4230577582","key":"scratch/aws/ec2/subnet","status":"applied"}
```

Run it again and it converges: the ledger says both keys are live, a live read agrees, and no create
is issued. The run still takes the lease and writes no journal, which is what a no-op looks like on
disk.

State lands under `cicd/work/iac-state` (gitignored), one file per key version — nothing is
overwritten in place:

```
ledger/scratch/aws/ec2/vpc.v1.json      {"phase":0,"proposed":"<intent>"}     pending
ledger/scratch/aws/ec2/vpc.v2.json      {"phase":1,"plan":…,"identity":…}     live
journal/<runID>.jsonl                   one line per attempted effect
```

`plan`, `proposed` and `identity` are base64 in the file: the ledger holds intent as opaque bytes and
never parses it.

### Failure

A step that fails compensates what the run already created, in reverse journal order, retrying
whatever the provider refuses until a pass makes no progress. A subnet CIDR outside the VPC range is
the easy way to see it:

```bash
./build/omnicli iac-provision --aws-region us-east-1 \
  --state cicd/work/iac-state --name failtest \
  --vpc-cidr 10.43.0.0/16 --subnet-cidr 192.168.1.0/24
```

```json
{"identity":"vpc-…","key":"failtest/aws/ec2/vpc","status":"applied, then compensated"}
{"key":"failtest/aws/ec2/subnet","status":"failed","error":"…","compensated":["failtest/aws/ec2/vpc"],"outstanding":[]}
```

A non-empty `outstanding` means the run is *partially* compensated — something it created is still
there and could not be removed.

### Limits

- **No destroy command.** Nothing tears down a successful run; delete by hand with
  `aws ec2 delete-subnet` then `delete-vpc`.
- **CIDRs are immutable in EC2**, so changing either is refused rather than replaced — no update
  path is declared for these exchanges.
- **Tags are stamped at create, not converged.** The read does not extract the tag set, so a tag
  edited outside the tool is not corrected.
- **Local disk only.** `O_EXCL` and `link` are unreliable on NFSv3; a network share is not a
  supported backing store.
