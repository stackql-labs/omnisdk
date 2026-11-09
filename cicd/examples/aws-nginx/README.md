# AWS nginx through merged EC2 documents

This example patches the **existing AWS EC2 provider document**, then executes a ledger-managed
mutation graph. The base registry is unchanged. It creates a VPC, two subnets, internet routing, an
SSH/HTTP security group, an imported SSH key, and one Amazon Linux 2023 VM serving a customised nginx
page. Teardown stops the VM, observes `stopped`, terminates it, observes `terminated`, and removes the
remaining resources in dependency order.

The local mock has passed the complete lifecycle. No real AWS deployment was performed during
implementation. The mock verifies request wiring, encoded bootstrap/key material, readiness polls,
ledger-backed cleanup and deletion constraints; it does not boot Linux or execute nginx.

## Build and prepare

Run from the repository root:

```sh
go build -o build/aws-nginx ./cicd/examples/aws-nginx

build/aws-nginx prepare \
  --case "$PWD/cicd/out/aws-nginx-live" \
  --registry "$PWD/test/corpus/registry" \
  --prefix your-unique-demo-name \
  --region ap-southeast-2 \
  --zone-a ap-southeast-2a --zone-b ap-southeast-2b \
  --image-id YOUR_AL2023_X86_64_AMI \
  --ssh-cidr YOUR_PUBLIC_IPV4/32

export CASE="$PWD/cicd/out/aws-nginx-live"
```

Choose an AL2023 **x86_64** AMI in the selected region and AZ names available to your account.
`--instance-type` defaults to `t3.micro`; it must match the AMI architecture. The example does not
choose a current AMI for you. The VPC CIDR is `10.77.0.0/16`, with `10.77.1.0/24` and `10.77.2.0/24`.

Preparation performs no AWS calls. By default, it generates `$CASE/id_ed25519` and its public key
using `ssh-keygen`. The private key remains local. Alternatively, supply `--public-key /path/key.pub`
and use your corresponding private key when connecting. Only the base64-encoded public key is sent
to AWS, through ImportKeyPair. [AWS key import contract](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_ImportKeyPair.html).

Review these prepared files before applying:

| File | Purpose |
| --- | --- |
| `config.json` | Account-independent settings, region, AMI, CIDRs, public key and collection name. |
| `ec2.merge.json` | RFC 7386 merge patch over `stackql_unstable_aws.ec2`. |
| `dependency-plan.json` | Create/delete operations, parameters and named incoming edges for every key. |
| `user-data.sh` | nginx installation, custom HTML, response header and health endpoint. |
| `id_ed25519` / `.pub` | Generated local SSH key pair, unless an existing public key was supplied. |

The default HTTP source is `0.0.0.0/0` on port 80; change it with `--http-cidr`. SSH is restricted to
your explicit IPv4 CIDR. The instance has a public IPv4 address and requires IMDSv2. Use EC2-capable
AWS credentials through the SDK's normal AWS authentication configuration. No IAM role is attached
to the VM, and the bootstrap needs no AWS credentials.

Applying this example creates billable EC2, EBS and public IPv4 resources. Keep the case directory
and its ledger until teardown succeeds. Run one phase per case at a time. Managed graphs renew
resource leases during execution and release resources after their last consumer finishes.
Collection-wide destruction retains an exclusive lease while scanning and removing dependencies.

## Create, inspect and verify

```sh
build/aws-nginx apply --case "$CASE"
build/aws-nginx status --case "$CASE"
build/aws-nginx verify --case "$CASE"
```

`apply` uses `omnisdk.EffectiveRegistry` with the prepared merge patch and runs
`ConvergeGraph` with a managed ledger key for each of the 13 owned resources/relationships.
It waits for VPC/subnet `available` and instance `running` before releasing dependents. It saves
`apply.jsonl`, `outputs.json`, the effective registry cache, ledger and write-ahead journals.

Automatic rollback is **off by default**. A failed apply preserves partial progress and the
ledger/journal for inspection or explicit teardown; this example does not opt into saga undo.
See [rollback policy](../../../docs/developer/resource-locking.md).

This is a **single-create graph**, not a general desired-state reconciliation example. A durable
`apply.started` guard prevents replaying creates after success or an uncertain failure. Use the same
case for inspection and destruction; do not remove the guard or replace the ledger to retry creation.

EC2 `running` does not establish that cloud-init or nginx has finished. `verify` separately polls
`http://PUBLIC_IP/healthz` for the deployment-specific marker for up to five minutes. The page at `/`
is customised, and responses include `X-Omnisdk-Deployment`.

For an interactive check, take the public IPv4 from `status`:

```sh
ssh -i "$CASE/id_ed25519" \
  -o StrictHostKeyChecking=accept-new \
  -o UserKnownHostsFile="$CASE/known_hosts" \
  ec2-user@PUBLIC_IP

# On the VM:
sudo cloud-init status --wait
sudo nginx -t
curl -fsS http://127.0.0.1/healthz
```

Bootstrap uses `dnf install -y nginx`, writes a complete nginx configuration and custom HTML, then
runs `nginx -t` and enables the service. User data is base64-encoded for the EC2 API.
[EC2 user-data contract](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/user-data.html).

## Stop and tear down

```sh
# Optional: observe the intermediate stopped state yourself.
build/aws-nginx stop --case "$CASE"
build/aws-nginx status --case "$CASE"

# Also performs the explicit stop/wait step if still needed.
build/aws-nginx destroy --case "$CASE"
```

Stop runs as a managed mutation under the instance's resource lock, preserving its resolved
intent and ownership/dependency record. The lock remains held through the stopped-state poll.
Destroy uses the collection ledger, not tags or a fresh list of matching AWS resources. The instance
must finish termination before its key is forgotten and its dependents can be removed. The ledger
then orders rule revocation/group deletion, route and association removal, gateway detachment,
subnet/route-table/gateway deletion, and VPC deletion according to the actual dependency DAG.
Independent branches may be removed concurrently or in a different valid order.

An outstanding deletion is an incomplete teardown, not success. Retain the ledger and rerun
`destroy` for provider-side dependency delays. Each destroy gets a fresh journal/transcript name.
For recovery after the instance was already terminated outside the workflow, `destroy --skip-stop`
skips the separate stop query and lets the ledger-backed termination/deletion phase handle it.
Unknown create outcomes are reported rather than adopted or guessed. If creation failed before
`outputs.json` was written, destruction still uses whatever confirmed identities the ledger holds.

The local private SSH key and logs are intentionally retained after cloud teardown. The AL2023 AMI
must have its normal root-volume `DeleteOnTermination` setting; the example attaches no extra disks.

## What the merge adds

The patch introduces isolated `web_*` resource aliases over the original EC2 operations. It does not
replace the provider or invent a second provisioning backend:

- Create/delete pairs for VPCs, subnets, gateways, gateway attachment, route table, default route,
  subnet associations, security group/rules, imported key and instance.
- The missing resource method for the existing `ImportKeyPair` API operation.
- Response transforms exposing precise create/observation handles.
- Explicit β bindings from returned IDs into DescribeVpcs, DescribeSubnets and DescribeInstances,
  with bounded waits for the corresponding states.
- Single-instance stop/terminate input declarations, retaining the original list parameter while
  explicitly declaring the required wire member `InstanceId.1` in this isolated overlay.

The graph adds named β data dependencies and a transform with hard-required routing/association
inputs before VM launch. Teardown derives dependencies and delete parameters from the ledger,
including indexed ingress-rule parameters.

## Repeatable mocked test

```sh
go build -o build/aws-nginx ./cicd/examples/aws-nginx
test/mock/.venv/bin/robot --outputdir test/robot/out/aws-nginx test/robot/aws_nginx.robot
```

The standard `test/robot/run.sh` builds this runner and discovers the suite. The test uses the actual
corpus plus merge, the actual SDK and ledger, and a strict local EC2 Query API. It checks:

- two subnets and both route-table associations;
- VPC/subnet availability before dependent creation;
- correct public-IP, subnet, SSH key and security-group inputs to RunInstances;
- custom nginx bootstrap and health marker in the encoded user data;
- running → stopping → stopped → shutting-down → terminated observations;
- original ingress rules available for revocation from ledger metadata;
- one instance launch, dependency-valid deletion, and no remaining mock cloud resources;
- durable pending intent and write locks at mutation/readiness checkpoints;
- early release of the VPC, security group and unused subnet association before VM provisioning;
- lost create responses: exactly one launch request, retained intent and dependencies, no blind replay;
- process termination during readiness: retained polling handle and refusal of competing writes;
- lost delete responses: retained identity/dependencies and no blind repeated termination.

Robot reports are under `test/robot/out/aws-nginx`. The temporary case path is included on failure;
its `api-transcript.json` and per-command stdout/stderr are retained for diagnosis.

See [resource locking and ledger recovery](../../../docs/developer/resource-locking.md) for the
lock scope, conservative recovery rules, and current limitations. Interrupted or uncertain runs
retain evidence; automatic reconciliation of those pending outcomes is not implemented here.
