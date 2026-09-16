
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

Every effect is compiled from the provider document that declares it, so `--registry` names the
document root and a new service is a document rather than code.

`--name` is the collection — the ledger key prefix, the lease scope, and the correlation tag stamped
on each object. `--state` is where that ledger lives; one state directory holds many collections.
Neither is defaulted: both decide which resources a run applies to.

```bash
./build/omnicli iac --registry test/corpus/registry --handle aws-vpc-subnet \
  --aws-region ap-southeast-2 --state cicd/work/iac-state --name scratch \
  --input '{"vpc_cidr":"10.42.0.0/16","subnet_cidr":"10.42.1.0/24","vpc_tags":{"Name":"omnisdk-demo-vpc"},"subnet_tags":{"Name":"omnisdk-demo-subnet"}}'
```

`./build/omnicli iac-handles` lists the precanned deployments and the inputs each takes.

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
./build/omnicli iac --registry test/corpus/registry --handle aws-vpc-subnet \
  --aws-region ap-southeast-2 --state cicd/work/iac-state --name failtest \
  --input '{"vpc_cidr":"10.43.0.0/16","subnet_cidr":"192.168.1.0/24"}' 
```

```json
{"identity":"vpc-…","key":"failtest/aws/ec2/vpc","status":"applied, then compensated"}
{"key":"failtest/aws/ec2/subnet","status":"failed","error":"…","compensated":["failtest/aws/ec2/vpc"],"outstanding":[]}
```

A non-empty `outstanding` means the run is *partially* compensated — something it created is still
there and could not be removed.

### Declared IaC, any provider

A blueprint is a convenience, not the mechanism. `iac-apply` takes the resources inline, so a service
with no blueprint needs no code: state the provider, the document address, and the residue the
document leaves unsaid.

```bash
./build/omnicli iac-apply test/corpus/registry '{
  "name": "scratch-two", "state": "cicd/work/iac-state",
  "resources": [
    {"key": "aws/ec2/vpc", "provider": "aws", "address": "ec2.vpcs",
     "desired": {"CidrBlock": "10.42.0.0/16"},
     "params": {"TagSpecification.1.ResourceType": "vpc",
                "TagSpecification.1.Tag.1.Key": "omnisdk:key"},
     "identity": "line_items.VpcId", "addressed_by": "VpcId",
     "correlation_param": "TagSpecification.1.Tag.1.Value"},
    {"key": "aws/ec2/subnet", "provider": "aws", "address": "ec2.subnets",
     "desired": {"CidrBlock": "10.42.1.0/24"},
     "inbound": [{"from": "aws/ec2/vpc", "as": "VpcId"}],
     "identity": "line_items.SubnetId", "addressed_by": "SubnetId"}
  ],
  "args": {"params": {"region": "ap-southeast-2"}}
}' --aws-region ap-southeast-2
```

`inbound` is the β edge: the VPC's recorded identity arrives as `VpcId`. Where the shape differs
from what the next call accepts, `via_type`/`via` reshape the inbox — the same `T_in` a query uses.

The three fields a document does not state:

| field | why the document cannot say it |
|---|---|
| `identity` | where the minted id sits in the **projected** response — `line_items.VpcId` after the schema-driven transform has run |
| `addressed_by` | which parameter carries an existing object's id, so a read or a delete is buildable from the ledger entry alone |
| `correlation_param` | which parameter takes the stamp linking the object back to its ledger key. Absent means losing the ledger orphans the resource |

**Google.** Client-named, so `identity` is empty — the caller chose the name and nothing has to be
read back out of the async Operation to know what was made. The intent goes in the request **body**,
which the document declares, so no field is scattered into the query:

```bash
./build/omnicli iac-apply test/corpus/registry '{
  "name": "scratch", "state": "cicd/work/iac-state",
  "resources": [
    {"key": "google/compute/network", "provider": "google", "address": "compute.networks",
     "desired": {"name": "demo-net", "autoCreateSubnetworks": false},
     "addressed_by": "network"}
  ],
  "args": {"params": {"project": "PROJECT"}}
}'
```

**Azure.** Also client-named, addressed by the resource name in the path:

```bash
./build/omnicli iac-apply test/corpus/registry '{
  "name": "scratch", "state": "cicd/work/iac-state",
  "resources": [
    {"key": "azure/network/vnet", "provider": "azure", "address": "network.virtual_networks",
     "desired": {"location": "eastus",
                 "properties": {"addressSpace": {"addressPrefixes": ["10.42.0.0/16"]}}},
     "params": {"resource_group_name": "RG", "virtual_network_name": "demo-vnet"},
     "addressed_by": "virtual_network_name"}
  ],
  "args": {"params": {"subscription_id": "SUBSCRIPTION"}}
}'
```

> **Verified:** the AWS case end to end against real EC2, and the Google body shape against a
> stand-in (`TestGoogleCreateSendsTheIntentAsABody`). Azure's declaration follows its document's
> signature and has not been run. GCP creates return an async Operation, so a run reports success
> once the call is accepted — waiting for completion needs an α edge and is not built.

### Calling it from Go

The CLI is a thin consumer; a client such as stackql uses the same facade:

```go
res := []omnisdk.ManagedResource{
    omnisdk.NewResource(
        "aws/ec2/vpc",              // key within the collection
        "aws", "ec2.vpcs",          // registry provider, document address
        []byte(`{"CidrBlock":"10.42.0.0/16"}`),
        map[string]string{"TagSpecification.1.ResourceType": "vpc"},
        nil, "", "",                // inbound, via type, via program
        "line_items.VpcId", "VpcId", "TagSpecification.1.Tag.1.Value",
    ),
}

pl, err := omnisdk.Converge(registry, "scratch", state, runID, res, omnisdk.Args{
    Params: map[string]string{"region": "ap-southeast-2"},
})
rows, err := pl.Open(ctx)          // same Plan/Rows a query returns
for rows.Next() { rows.Row() }     // {"key":…, "identity":…, "status":…}
```

`Converge` returns the same `Plan`/`Rows` a query does, so a consumer iterates one cursor shape
whether it is reading or provisioning.

**Auth is identical to every other call.** `Args` carries it, resolved exactly as a query resolves
it: the provider document declares the scheme — `aws_signing_v4`, `service_account`, `oauth2` — and
the credential comes from `Args.Auth`, falling back to the canonical environment variables. A
provisioning run signs, or exchanges a token, by the same code path a read does, so a consumer that
can already query a provider can already provision against it.

```go
omnisdk.Converge(registry, name, state, runID, res, omnisdk.Args{
    // Auth is optional: nil falls back to the canonical AWS_*, AZURE_* and GOOGLE_* variables.
    Auth:   &omnisdk.Auth{AccessKeyID: "...", SecretAccessKey: "..."},
    Params: map[string]string{"region": "ap-southeast-2"},   // scope: required, never inferred
})
```

Only the credential a document actually declares is required: a run touching AWS alone does not fail
because a Google key elsewhere is stale, and a credential that IS present but unusable says so rather
than reporting as absent.

Blueprints are reachable the same way:

```go
bp, ok := omnisdk.BlueprintFor("aws-vpc-subnet")
res, err := bp.Resources(map[string]string{"region": "ap-southeast-2", "vpc_cidr": "10.42.0.0/16", ...})
```

### Limits

- **No destroy command.** Nothing tears down a successful run; delete by hand with
  `aws ec2 delete-subnet` then `delete-vpc`.
- **CIDRs are immutable in EC2**, so changing either is refused rather than replaced — no update
  path is declared for these exchanges.
- **Tags are stamped at create, not converged.** The read does not extract the tag set, so a tag
  edited outside the tool is not corrected.
- **Local disk only.** `O_EXCL` and `link` are unreliable on NFSv3; a network share is not a
  supported backing store.
