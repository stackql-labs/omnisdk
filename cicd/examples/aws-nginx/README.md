# Declarative AWS nginx deployment

The deployment is [resources.json](resources.json). API operations, self reads, readiness polls and
the stopped-state update are declared in [ec2.merge.json](ec2.merge.json), merged over the existing
AWS EC2 document. [stopped.json](stopped.json) declares the instance's final pre-deletion state.
The generic `omnicli iac-apply` planner builds the graph from these files. There is no example-specific
Go executable or deployment graph builder.

The spec declares a VPC, two subnets, internet gateway and attachment, route table and default route,
two subnet associations, security group and ingress rules, SSH key pair, and one nginx VM. Its
`inbound` members supply identities and dependencies. The instance's Starlark inbox transform maps
those identities to EC2 inputs; routing dependencies must complete before it can launch.

The full lifecycle, unchanged reapply and failure cases have been tested against a local EC2 mock.
**Real AWS execution has not been verified.** The mock checks API wiring and bootstrap content; it
cannot prove that Linux boots or nginx installs successfully.

## Prepare local inputs and merge the provider document

Run from the repository root. Preparation resolves Amazon Linux 2023 **x86_64** once through
AWS SSM, then writes the concrete regional AMI ID into `inputs.json`. This read requires the AWS
CLI and credentials; `--image-id` with a concrete AMI ID skips the lookup. Choose availability zones
available to your account. The commands determine your public IPv4 for SSH.

```sh
go build -o build/omnicli ./cmd/omnicli
export CASE="$PWD/cicd/out/nginx-live"
export REGION=ap-southeast-2
export COLLECTION=my-nginx-demo
export SSH_CIDR="$(curl --fail --silent --show-error https://checkip.amazonaws.com)/32"

python3 cicd/examples/aws-nginx/prepare.py \
  --case "$CASE" --prefix "$COLLECTION" --region "$REGION" \
  --zone-a ap-southeast-2a --zone-b ap-southeast-2b \
  --ssh-cidr "$SSH_CIDR"

export REGISTRY="$(build/omnicli doc-merge test/corpus/registry "@$CASE/merge.json")"
```

The SSM parameter is `/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64`.
The caller needs `ssm:GetParameter` for this preparation lookup, in addition to the EC2 permissions
for deployment. Launches receive the persisted `ami-...` value, never a moving `resolve:ssm` reference.

Each prepared deployment gets a unique client token, persisted in `inputs.json`. Reapply and retries
reuse that token and the pinned image. Preparation refuses to overwrite existing inputs. A new
case is a new deployment generation; reusing a display prefix does not reuse its client token.
Do not rotate a token to bypass an unresolved launch: first establish the old launch's outcome.

Preparation generates a local SSH key pair, encodes the public key and [user-data.sh](user-data.sh),
and writes binding values to `$CASE/inputs.json`. Supply `--public-key /path/key.pub` to import an
existing key instead. The private key remains local. This script performs only the optional read-only SSM lookup; it neither
constructs nor executes a deployment graph.

`doc-merge` applies the RFC 7386 overlay through `omnisdk.EffectiveRegistry`, caches the effective
registry under `$CASE/registry-cache`, and prints its path. The source registry is unchanged.
Review `resources.json`, `stopped.json`, `ec2.merge.json`, `$CASE/inputs.json` and `$CASE/user-data.sh`
before applying. Credentials use the SDK's normal AWS authentication configuration.

Defaults: `t3.micro`, VPC `10.77.0.0/16`, subnets `10.77.1.0/24` and `10.77.2.0/24`, public IPv4,
IMDSv2 required, HTTP open on port 80, and SSH restricted to your supplied CIDR. Use `--http-cidr`
to restrict HTTP. The VM has no IAM instance profile. Applying creates billable AWS resources.

## Apply the desired-state spec

```sh
build/omnicli iac-apply "$REGISTRY" \
  @cicd/examples/aws-nginx/resources.json \
  --input "@$CASE/inputs.json" --aws-region "$REGION" \
  --name "$COLLECTION" --state "$CASE/state" --run-id create-001 \
  --out "$CASE/create.jsonl"
```

The generic engine resolves input references, lowers the resource spec, waits for the document's
VPC/subnet/VM readiness predicates, and records identity and dependency metadata in the ledger.
A failed or outstanding IaC result produces a nonzero CLI exit status while retaining JSONL output.
Automatic rollback defaults off; see [resource locking](../../../docs/developer/resource-locking.md).

Run the same spec again with a **fresh run ID** to verify that unchanged objects are read rather
than created again:

```sh
build/omnicli iac-apply "$REGISTRY" \
  @cicd/examples/aws-nginx/resources.json \
  --input "@$CASE/inputs.json" --aws-region "$REGION" \
  --name "$COLLECTION" --state "$CASE/state" --run-id check-002 \
  --out "$CASE/check.jsonl"
```

This example deliberately puts creation inputs in `write_once`; it does not claim to reconcile
changes to AMI, subnet CIDRs, SSH rules or instance size. Changing those inputs is not a supported
update plan. The separately declared `State: stopped` update is supported. Keep the same collection,
state directory, effective registry and inputs for the entire deployment.

## Read the ledger in stackql

With Omni's read-only ledger API available in the consumed release:

```sql
SELECT resource_key, identity, phase
FROM stackql_preview.iac.ledger
WHERE collection = 'my-nginx-demo' AND resource_key = 'instance';
```

This reads the same ledger root used by apply. `pending` means uncertain; any identity there is
from the previous committed state. No leases, journal entries or cloud requests are made.
`Name` tags in this example are explicit display labels, not ownership markers or WAL.

## Inspect and verify the VM

The following extraction uses `jq`; alternatively read the instance's identity from `create.jsonl`.

```sh
export INSTANCE_ID="$(jq -r 'select(.key | endswith("/instance")) | .identity' "$CASE/create.jsonl")"

build/omnicli doc-run "$REGISTRY" stackql_unstable_aws.ec2.web_instance \
  "{\"params\":{\"InstanceId.1\":\"$INSTANCE_ID\"}}" \
  --aws-region "$REGION" --out "$CASE/instance.jsonl"

export PUBLIC_IP="$(jq -r '.public_ip' "$CASE/instance.jsonl")"
curl --fail "http://$PUBLIC_IP/healthz"
ssh -i "$CASE/id_ed25519" ec2-user@"$PUBLIC_IP"
```

EC2 `running` does not establish that cloud-init has installed nginx. Retry the HTTP check while
bootstrap completes; it should return `omnisdk COLLECTION ready`. The root page is customised.
Over SSH, inspect `sudo cloud-init status --wait` and `sudo nginx -t` if needed. When importing an
existing public key, use its corresponding private key for SSH.

## Declare stopped, then destroy in dependency order

```sh
build/omnicli iac-apply "$REGISTRY" \
  @cicd/examples/aws-nginx/stopped.json \
  --input "@$CASE/inputs.json" --aws-region "$REGION" \
  --name "$COLLECTION" --state "$CASE/state" --run-id stop-003 \
  --out "$CASE/stop.jsonl"

build/omnicli iac-destroy --registry "$REGISTRY" \
  --aws-region "$REGION" --name "$COLLECTION" --state "$CASE/state" \
  --run-id destroy-004 --out "$CASE/destroy.jsonl"
```

The stopped-state spec is lowered as an update; the provider overlay selects `StopInstances` and
polls for `stopped`. Reapplying that same spec is a no-op. Its declared dependencies remain recorded.
`iac-destroy` uses ledger identities and dependencies, terminates the instance and waits for
`terminated`, then removes dependents before their parents. The CLI contains no nginx-specific
ordering. Local keys, inputs and logs are retained after cloud cleanup.

Use a fresh run ID for every execution, including retries. Pending/unknown outcomes must be
investigated before further mutation; do not remove ledger entries or replay uncertain creates.
Inspect every outstanding result and retain the state directory until teardown is confirmed.

## How to run this in stackql

Start with **your existing pinned canonical AWS registry** in stackql. The session merge augments
that registry; do not point stackql at Omni's test corpus or its generated effective registry.
Requires stackql's `document_merge` and `inputs_file` support, consuming Omni `v0.1.11-alpha01`.
These stackql steps have not been verified against live AWS.

Prepare the local key and bootstrap inputs as above; skip `omnicli doc-merge`. Keep the prepared
files in `cicd/out/nginx-live`. From this repository root, open stackql using your usual pinned
registry configuration, adding the preview flag:

```sh
./build/stackql shell --preview '{"unstable":true}'
```

Inside that session, merge the overlay and create the deployment:

```sql
SET "$.document_merge" = '@cicd/out/nginx-live/merge.json';

SELECT * FROM stackql_preview.iac.run
WHERE collection = 'my-nginx-demo'
  AND region = 'ap-southeast-2'
  AND resources_file = 'cicd/examples/aws-nginx/resources.json'
  AND inputs_file = 'cicd/out/nginx-live/inputs.json';
```

Use the collection and region chosen during preparation. AWS credentials use the normal environment.
Run the same SELECT again to verify unchanged reapply. Each execution gets a fresh run ID automatically.
Inspect its results and verify nginx/SSH before proceeding.

Declare the VM stopped, then destroy the collection in dependency order:

```sql
SELECT * FROM stackql_preview.iac.run
WHERE collection = 'my-nginx-demo'
  AND region = 'ap-southeast-2'
  AND resources_file = 'cicd/examples/aws-nginx/stopped.json'
  AND inputs_file = 'cicd/out/nginx-live/inputs.json';

-- Execute only after the stopped-state result succeeds.
SELECT * FROM stackql_preview.iac.destroy
WHERE collection = 'my-nginx-demo'
  AND region = 'ap-southeast-2';

-- After teardown, restore the session's canonical document view.
UNSET "$.document_merge";
```

Ledger and journals default to `.stackql/iac` under the working directory (or `--approot`). Merge cache
normally defaults to `.stackql/cache/document-views`; preparation's `merge.json` explicitly selects
its case-local `registry-cache`. Keep the same app root, collection and overlay for every run.
Reopening a session requires repeating SET before continuing. Paths refer to the stackql process.

**If continuing an existing omnicli deployment**, start the shell with
`--preview '{"unstable":true,"iacStateRoot":"cicd/out/nginx-live/state"}'` instead, using that
existing ledger throughout. A different ledger can create duplicates. Inspect failed/outstanding
results before any further mutation.

## Recover a verified pending outcome

After independently verifying that a failed create did not happen **and is no longer in flight**,
read its `phase` and `version` from `stackql_preview.iac.ledger`. Then, using the returned version:

```sql
SELECT * FROM stackql_preview.iac.recover
WHERE collection = 'my-nginx-demo'
  AND resource_key = 'subnet_a'
  AND outcome = 'absent'
  AND version = '1';
```

`1` is the example's first pending version; use the actual observed value. Recovery makes no cloud
calls. It records your assertion, checks the version under an exclusive resource lock and clears
only an unresolved first create with no retained dependents. Reapply the same spec afterwards;
completed resources remain managed. For a verified completed create whose response was lost,
use `outcome = 'applied'` with its independently verified `identity` instead. Existence alone does
not establish readiness or successful completion. Never infer either outcome from tags.
Requires the new Omni recovery release and corresponding stackql integration.

## Mock verification

```sh
go build -o build/omnicli ./cmd/omnicli
test/mock/.venv/bin/robot --outputdir test/robot/out/aws-nginx test/robot/aws_nginx.robot
```

Robot runs **these same files and generic CLI commands** against an EC2 Query mock. It checks
create, unchanged reapply, declared stop, dependency-ordered deletion, durable ledger checkpoints,
lost create/delete responses, and process interruption during readiness. Temporary case directories
retain API transcripts, checkpoint snapshots and command output. No real AWS resources are created
by these tests.
