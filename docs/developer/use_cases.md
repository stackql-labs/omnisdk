
# Use cases


Secrets and build setup are as in [the developer guide](/docs/developer/developer_guide.md). Some of it is repeated here for ease of copy and paste, but not all; refer to the developer guide for the full build and auth setup.

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

`--name` is the collection — the handle a later run uses to address the same resources, and the
correlation tag stamped on each object. `--state` is where that ledger lives; one state directory holds many collections.
Neither is defaulted: both decide which resources a run applies to.

```bash
./build/omnicli iac --registry test/corpus/registry --handle aws-vpc-subnet \
  --aws-region "${_AWS_REGION}" --state cicd/work/iac-state --name scratch \
  --input '{"vpc_cidr":"10.42.0.0/16","subnet_cidr":"10.42.1.0/24","vpc_tags":{"Name":"scratch"}}'
```

`./build/omnicli iac-handles` lists the precanned deployments and the inputs each takes.

```json
{"identity":"vpc-08e599d96e6bbe5fd","key":"scratch/aws/ec2/vpc","status":"applied"}
{"identity":"subnet-08300cf4230577582","key":"scratch/aws/ec2/subnet","status":"applied"}
```

Run it again and it converges: the ledger says both keys are live, a live read agrees, and no create
is issued, and no journal is written.

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
  --aws-region "${_AWS_REGION}" --state cicd/work/iac-state --name failtest \
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
  "name": "scratch", "state": "cicd/work/iac-state",
  "resources": [
    {"key": "aws/ec2/vpc", "provider": "aws", "address": "ec2.vpcs",
     "desired": {"CidrBlock": "10.42.0.0/16"},
     "identity": "line_items.VpcId", "addressed_by": "VpcId"},
    {"key": "aws/ec2/subnet", "provider": "aws", "address": "ec2.subnets",
     "desired": {"CidrBlock": "10.42.1.0/24"},
     "inbound": [{"from": "aws/ec2/vpc", "as": "VpcId"}],
     "identity": "line_items.SubnetId", "addressed_by": "SubnetId"}
  ],
  "args": {"params": {"region": "'"${_AWS_REGION}"'"}}
}'
```

`inbound` is the β edge: the VPC's recorded identity arrives as `VpcId`. Where the shape differs
from what the next call accepts, `via_type`/`via` reshape what arrives — the same `T_in` a query
uses.

Resource identity and optional debugging configuration:

| field | why the document cannot say it |
|---|---|
| `identity` | where the minted id sits in the **projected** response — `line_items.VpcId` after the schema-driven transform has run |
| `addressed_by` | which parameter carries an existing object's id, so a read or a delete is buildable from the ledger entry alone |
| `debug_correlation_param` | optional debug-label configuration: `{"target":"request parameter", "transformer":"standard"}` |

A nonempty `target` opts in to a debug label. Blank or `standard` transformer produces
`<collection>/<resource key>`. Omission, `{}`, or an empty target requests no label:
none is created, and an existing label on the ledger-addressed resource is removed.
Unknown transformers and the old `correlation_param` key are errors. The provider document
must declare the creation inputs, live attribute and update/removal route for that target;
unsupported targets fail planning. Debug labels never recover state or establish ownership.

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
  "args": {"params": {"project": "'"${_GOOGLE_PROJECT_ID}"'"}}
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
     "params": {"resource_group_name": "'"${_AZURE_RESOURCE_GROUP}"'", "virtual_network_name": "demo-vnet"},
     "addressed_by": "virtual_network_name"}
  ],
  "args": {"params": {"subscription_id": "'"${AZURE_SUBSCRIPTION_ID}"'"}}
}'
```

### Tagged buckets, run for real

One tagged bucket per cloud. Each command creates the bucket on the first run, reads it and changes
nothing on a rerun, and puts the tags back if they are edited elsewhere. Credentials and scope come
from `secrets.sh`; bucket and account names are global, so they are derived from your project and
subscription. The Azure resource group must already exist:

```bash
source cicd/vol/vendor-secrets/secrets.sh
export _AZURE_RESOURCE_GROUP='an-existing-resource-group'
export _OMNI_BUCKET="omni-demo-${_GOOGLE_PROJECT_ID}"
export _OMNI_STORAGE_ACCOUNT="omni$(printf %s "${AZURE_SUBSCRIPTION_ID}" | tr -d - | cut -c1-16)"
```

Run everything below in that same shell.

Azure, once, as the subscription's owner — lets the app create storage accounts:

```bash
az login
az role assignment create --assignee "${AZURE_CLIENT_ID}" --role "Storage Account Contributor" \
  --scope "/subscriptions/${AZURE_SUBSCRIPTION_ID}/resourceGroups/${_AZURE_RESOURCE_GROUP}"
```

**GCS** — labels on the bucket:

```bash

export _OMNI_BUCKET='omni-iac-trial-bucket-01' # must be unique in each provider


./build/omnicli iac-apply test/corpus/registry '{
  "name": "buckets", "state": "cicd/work/iac-state",
  "resources": [
    {"key": "google/storage/bucket", "provider": "google", "address": "storage.buckets",
     "desired": {"name": "'"${_OMNI_BUCKET}"'", "labels": {"env": "dev", "team": "platform"}},
     "identity": "name", "addressed_by": "bucket"}
  ],
  "args": {"params": {"project": "'"${_GOOGLE_PROJECT_ID}"'"}}
}'
```

**Azure** — a tagged storage account and a blob container in it with metadata (containers take
metadata, not tags; the portal shows it under the container's Properties):

```bash
./build/omnicli iac-apply test/corpus/registry '{
  "name": "buckets", "state": "cicd/work/iac-state",
  "resources": [
    {"key": "azure/storage/account", "provider": "azure", "address": "storage.storage_accounts",
     "desired": {"location": "eastus", "kind": "StorageV2", "sku": {"name": "Standard_LRS"},
                 "tags": {"env": "dev", "team": "platform"}},
     "params": {"resource_group_name": "'"${_AZURE_RESOURCE_GROUP}"'", "account_name": "'"${_OMNI_STORAGE_ACCOUNT}"'"},
     "identity": "name", "addressed_by": "account_name"},
    {"key": "azure/storage/container", "provider": "azure", "address": "storage.blob_containers",
     "desired": {"properties": {"metadata": {"env": "dev", "team": "platform"}}},
     "params": {"resource_group_name": "'"${_AZURE_RESOURCE_GROUP}"'", "container_name": "omni-demo"},
     "inbound": [{"from": "azure/storage/account", "as": "account_name"}],
     "identity": "name", "addressed_by": "container_name"}
  ],
  "args": {"params": {"subscription_id": "'"${AZURE_SUBSCRIPTION_ID}"'"}}
}'
```

**S3** — the bucket, and its tags as a resource of their own that reads the bucket's name:

```bash
./build/omnicli iac-apply test/corpus/registry '{
  "name": "buckets", "state": "cicd/work/iac-state",
  "resources": [
    {"key": "aws/s3/bucket", "provider": "aws", "address": "s3.buckets",
     "desired": {}, "params": {"bucket": "'"${_OMNI_BUCKET}"'"},
     "identity": "Name", "addressed_by": "bucket"},
    {"key": "aws/s3/bucket-tags", "provider": "aws", "address": "s3.bucket_taggings",
     "desired": {"Tagging": {"TagSet": {"Tag": [{"Key": "env", "Value": "dev"},
                                                  {"Key": "team", "Value": "platform"}]}}},
     "inbound": [{"from": "aws/s3/bucket", "as": "bucket"}],
     "addressed_by": "bucket"}
  ],
  "args": {"params": {"region": "'"${_AWS_REGION}"'"}}
}'
```

Outside `us-east-1`, S3 needs the region in the bucket's own body as well:
`"desired": {"CreateBucketConfiguration": {"LocationConstraint": "eu-west-1"}}`.

The three can share one collection, as here, or each have its own. To remove everything the
collection made, dependents first — the S3 tags before their bucket:

```bash
./build/omnicli iac-destroy --registry test/corpus/registry --name buckets \
  --state cicd/work/iac-state \
  --params '{"project":"'"${_GOOGLE_PROJECT_ID}"'","subscription_id":"'"${AZURE_SUBSCRIPTION_ID}"'","region":"'"${_AWS_REGION}"'"}'
```

Each row says what happened to a key: `destroyed`, or `outstanding` with the provider's reason for
refusing — a bucket that still holds objects, say.

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

pl, err := omnisdk.Prepare(registry, omnisdk.DesiredState(res), omnisdk.NewLedger("scratch", state, runID), omnisdk.Args{
    Params: map[string]string{"region": "us-east-1"},
})
rows, err := pl.Open(ctx)          // same Plan/Rows a query returns
for rows.Next() { rows.Row() }     // {"key":…, "identity":…, "status":…}
```

A converging run returns the same `Plan`/`Rows` a query does, so a consumer iterates one cursor shape
whether it is reading or provisioning.

**Auth is identical to every other call.** `Args` carries it, resolved exactly as a query resolves
it: the provider document declares the scheme — `aws_signing_v4`, `service_account`, `oauth2` — and
the credential comes from the provider's entry in `Args.AuthByProvider`, falling back to the canonical environment variables. A
provisioning run signs, or exchanges a token, by the same code path a read does, so a consumer that
can already query a provider can already provision against it.

```go
omnisdk.Prepare(registry, omnisdk.DesiredState(res), omnisdk.NewLedger(name, state, runID), omnisdk.Args{
    // Auth is per provider and optional: a provider absent here falls back to the canonical AWS_*,
    // AZURE_* and GOOGLE_* variables. One provider's credential never reaches another.
    AuthByProvider: map[string]*omnisdk.Auth{"aws": {AccessKeyID: "...", SecretAccessKey: "..."}},
    Params: map[string]string{"region": "us-east-1"},   // scope: required, never inferred
})
```

Only the credential a document actually declares is required: a run touching AWS alone does not fail
because a Google key elsewhere is stale, and a credential that IS present but unusable says so rather
than reporting as absent.

Blueprints are reachable the same way:

```go
bp, ok := omnisdk.BlueprintFor("aws-vpc-subnet")
res, err := bp.Resources(map[string]string{"region": "us-east-1", "vpc_cidr": "10.42.0.0/16", ...})
```

### Examples

Each example is a test against a fake provider, named beside it, so the code shown is the code that
runs.

**The document says when a create is finished.** A Google network create answers with an Operation
before the network exists. A session patch declares that, so anything wired to the create waits for
it — the graph says nothing about waiting:

```go
awaitNetworks := omnisdk.DocPatch{
    Service: "stackql_unstable_google.compute",
    Merge: json.RawMessage(`{"components":{"x-stackQL-resources":{
      "global_operations":{"methods":{"get":{
        "operation":{"$ref":"#/paths/~1projects~1{project}~1global~1operations~1{operation}/get"},
        "response":{"mediaType":"application/json","openAPIDocKey":"200"}}}},
      "networks":{"methods":{"insert":{"x-stackQL-beta":{
        "method":{"$ref":"#/components/x-stackQL-resources/global_operations/methods/get"},
        "bind":{"operation":"name"},
        "await":{"statusPath":"status","done":"DONE","errorPath":"error",
                 "interval":"2s","maxAttempts":60}}}}}}}}`),
}
view, err := omnisdk.EffectiveRegistry(registry, []omnisdk.DocPatch{awaitNetworks}, omnisdk.DocCache{Dir: cache})

g, err := omnisdk.NewGraph(
    []omnisdk.Node{
        omnisdk.NewMutationNode("net", "stackql_unstable_google.compute.networks", "insert",
            map[string]string{"project": "demo"}, nil,
            map[string]any{"name": "net-1", "autoCreateSubnetworks": false}),
        omnisdk.NewMutationNode("sub", "stackql_unstable_google.compute.subnetworks", "insert",
            map[string]string{"project": "demo", "region": "us-east1"}, nil,
            map[string]any{"name": "sub-1", "ipCidrRange": "10.0.0.0/24", "network": nil}),
    },
    []omnisdk.Wiring{omnisdk.NewWiring("sub",
        []omnisdk.Inbound{omnisdk.NewInbound("net", "targetLink", "network")}, "", "")},
)
pl, err := omnisdk.Prepare(view, omnisdk.GraphStatement(g), omnisdk.NoWAL(), args)
```

The network is created, its operation is polled until `DONE`, and only then is the subnetwork
created, carrying the `targetLink` only a finished operation reports. An operation that never reports
done fails with `ErrOutcomeUnknown` — the network may exist — and one done with an `error` fails with
it; either way the subnetwork is not created. Without the patch the subnetwork is sent at once with
`network: null`. `view` is the session's handle: built once, passed to every query after.
(`TestBetaAwaitsTheCreateBeforeItsDependent`, `TestBetaUnfinishedOrFailedStopsTheDependent`.)

**A rerun costs a read; drift is fixed by the call that fixes it.** A VPC converged from desired
state:

```go
vpc := omnisdk.NewResource("aws/ec2/vpc", "aws", "ec2.vpcs",
    []byte(`{"CidrBlock":"10.42.0.0/16","InstanceTenancy":"default"}`),
    map[string]string{"TagSpecification.1.ResourceType": "vpc", "TagSpecification.1.Tag.1.Key": "omnisdk:key"},
    nil, "", "", "line_items.VpcId", "VpcId", "TagSpecification.1.Tag.1.Value")
pl, err := omnisdk.Prepare(registry, omnisdk.DesiredState([]omnisdk.ManagedResource{vpc}),
    omnisdk.NewLedger("demo", state, runID), args)
```

The first run calls `CreateVpc`. A rerun calls `DescribeVpcs` and nothing else. After the tenancy is
changed in the console, the next run calls `DescribeVpcs` then `ModifyVpcTenancy` with the VPC id and
the tenancy alone: of the five operations the document binds to update, it is the only one that
accepts `InstanceTenancy`, and an operation without a body is sent only what differs.
(`TestExampleVPCConvergesAndCorrectsDrift`.)

**A tagged bucket on each cloud.** One resource per cloud, converged three times: the first run
creates, the second only reads, and after the tags are changed outside the tool the third puts them
back.

```go
gcs := omnisdk.NewResource("google/storage/bucket", "google", "storage.buckets",
    []byte(`{"name":"omni-demo","labels":{"env":"dev","team":"platform"}}`),
    nil, nil, "", "", "name", "bucket", "")

azure := omnisdk.NewResource("azure/storage/account", "azure", "storage.storage_accounts",
    []byte(`{"location":"eastus","kind":"StorageV2","sku":{"name":"Standard_LRS"},"tags":{"env":"dev"}}`),
    map[string]string{"resource_group_name": "rg", "account_name": "omnidemo"},
    nil, "", "", "name", "account_name", "")

s3 := omnisdk.NewResource("aws/s3/bucket", "aws", "s3.buckets", []byte(`{}`),
    map[string]string{"bucket": "omni-demo"}, nil, "", "", "Name", "bucket", "")
s3Tags := omnisdk.NewResource("aws/s3/bucket-tags", "aws", "s3.bucket_taggings",
    []byte(`{"Tagging":{"TagSet":{"Tag":[{"Key":"env","Value":"dev"}]}}}`),
    nil, []omnisdk.Arrival{{From: "aws/s3/bucket", As: "bucket"}}, "", "", "", "bucket", "")
```

Each cloud differs only in what its document offers:

| | create | read back | tags corrected by |
|---|---|---|---|
| GCS | `insert` | `get` by name | `patch` |
| Azure | `create` (PUT) | listing the resource group, matched on `name` — the document has no get | `update` (PATCH) |
| S3 | `create_bucket` (PUT) | listing buckets, matched on `Name` | `put_bucket_tagging`, a resource of its own with an XML body |

A caller-named object's identity is the name it was given, where the response carries none — S3's
create answers with an empty body. S3's tags are written again on every run: what S3 reads back for
them is not the document written, so they never compare equal; the PUT is whole, so the tags are the
same. (`TestExampleGCSBucket`, `TestExampleAzureStorageAccount`, `TestExampleS3BucketWithTags`.)

**A failed run undoes itself.** Two widgets converged as one graph run, the second refused by the
provider: the first is deleted, and the final row says so — `{"status":"failed",
"compensated":["demo/w1"]}`. (`TestConvergeGraphCompensatesAFailedRun`.)

### Limits

- **A create is awaited only where its document declares an `x-stackQL-beta`.** The base Google
  documents declare none, so without a patch a create is reported once the call is accepted.
- **Destroy deletes what desired state made.** `./build/omnicli iac-destroy --registry … --name … --state …`
  removes the collection's keys, dependents first. A delete the provider refuses — deletion
  protection, a lock — is reported outstanding with its reason, never forced.
- **Drift goes to any update operation accepting the changed field.** EC2 `CidrBlock` drift is sent
  to `AssociateVpcCidrBlock`, which adds a CIDR block rather than changing it; nothing yet declares a
  field immutable, and nothing replaces an object.
- **Tags given as create parameters are not converged.** The read returns them, but
  `TagSpecification.*` params are not desired state, so a tag edited elsewhere is not compared or
  put back. EC2 declares tags as their own resource (`ec2.tags`: `CreateTags`, `DeleteTags`).
- **Local disk only.** `O_EXCL` and `link` are unreliable on NFSv3; a network share is not a
  supported backing store.
