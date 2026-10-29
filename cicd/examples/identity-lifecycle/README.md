# Identity lifecycle walkthroughs

These examples create real cloud resources when you run `iac-apply`. Preparation only writes files.
Use a fresh output directory and collection for each profile. Keep the same state directory throughout
that profile's lifecycle. Never replace the ledger between stages.

| Profile | Resources | Identity contract | Repeated update |
| --- | --- | --- | --- |
| `aws` | VPC and dependent subnet | Provider-generated IDs in the create response | Ordinary `resource-key` tag value |
| `await-eager` | Two Google custom-mode VPC networks | Extract the network name from an initial operation's `targetLink`, if present; still await completion | Network routing mode |
| `await-late` | Two Google custom-mode VPC networks | Extract the network name only from the completed operation | Network routing mode |
| `caller-name` | Two empty Google Storage buckets | Caller chooses names; the returned resource confirms them | Bucket labels |

The classes overlap: Google networks are caller-named and asynchronous. These two network profiles
exercise different publication points using explicit response transforms, not different cloud behavior.
An operation's own name is a polling handle, not the created resource's identity. `await-eager`
preserves an early ID when the final response omits it; dependent resources still wait for completion.
The ledger marks the effect live only after success. A failed or timed-out await remains uncertain;
this implementation does not persist a resumable operation handle for restarting a poll after a crash.

Google documents are intentionally small, isolated declarations for these examples. They use bearer
auth from `OMNI_LIFECYCLE_TOKEN`. No source provider documents are modified. The Google network
pair is independent; the AWS example and core Robot cases exercise parent/child dependencies.

## Prepare one profile

Build once:

```sh
go build -o build/omnicli ./cmd/omnicli
```

AWS, with credentials already configured in your terminal:

```sh
python3 cicd/examples/identity-lifecycle/prepare.py --profile aws \
  --region ap-southeast-2 --prefix demo \
  --out cicd/out/identity-aws
export CASE="$PWD/cicd/out/identity-aws"
export COLLECTION=identity-aws
export DELETE_KEY=aws/ec2/subnet
export SCOPE='{"region":"ap-southeast-2"}'
```

For Google, choose a project and unique lowercase name prefix. Bucket names must be globally unique.
Enable the Compute Engine or Cloud Storage API in that project before using its profile. Authenticate
with your chosen Google identity; refresh this token if it expires during the walkthrough:

```sh
export OMNI_LIFECYCLE_TOKEN="$(gcloud auth print-access-token)"
export PROJECT=your-project-id
export PREFIX=your-unique-prefix
```

Await with an early identity:

```sh
python3 cicd/examples/identity-lifecycle/prepare.py --profile await-eager \
  --project "$PROJECT" --location australia-southeast1 --prefix "$PREFIX" \
  --out cicd/out/identity-await-eager
export CASE="$PWD/cicd/out/identity-await-eager"
export COLLECTION=identity-await-eager
export DELETE_KEY=secondary
export SCOPE="{\"project\":\"$PROJECT\"}"
```

Await with a late identity:

```sh
python3 cicd/examples/identity-lifecycle/prepare.py --profile await-late \
  --project "$PROJECT" --location australia-southeast1 --prefix "$PREFIX" \
  --out cicd/out/identity-await-late
export CASE="$PWD/cicd/out/identity-await-late"
export COLLECTION=identity-await-late
export DELETE_KEY=secondary
export SCOPE="{\"project\":\"$PROJECT\"}"
```

Caller-supplied names:

```sh
python3 cicd/examples/identity-lifecycle/prepare.py --profile caller-name \
  --project "$PROJECT" --location AUSTRALIA-SOUTHEAST1 --prefix "$PREFIX" \
  --out cicd/out/identity-caller-name
export CASE="$PWD/cicd/out/identity-caller-name"
export COLLECTION=identity-caller-name
export DELETE_KEY=secondary
export SCOPE="{\"project\":\"$PROJECT\"}"
```

Use different prefixes if running both network profiles concurrently. The AWS prefix does not rename
its fixed resource keys; the collection separates deployments.

## Create, update three times, prune, update again, destroy

The same sequence applies to every profile above. `args.params` in these files supplies explicit
execution scope; resource-level `write_once` remains distinct from reconciled `desired`.

```sh
apply_stage() {
  build/omnicli iac-apply "$CASE/registry" "@$CASE/$1.json" \
    --name "$COLLECTION" --state "$CASE/state" --run-id "$2"
}

apply_stage 01-create create-001
apply_stage 01-create unchanged-001
apply_stage 02-update update-001
apply_stage 02-update unchanged-002
apply_stage 03-update update-002
apply_stage 03-update unchanged-003
apply_stage 04-update update-003
apply_stage 04-update unchanged-004
```

Record the two identities printed by create. They must remain unchanged through updates. Unchanged
applies should perform reads but no writes. The current `applied` output also covers no-ops; it does
not itself prove that no mutation occurred. Inspect the resources directly as described below.

Delete only the selected resource, then continue from the reduced declaration:

```sh
build/omnicli iac-destroy --registry "$CASE/registry" --name "$COLLECTION" \
  --state "$CASE/state" --run-id prune-001 --params "$SCOPE" --key "$DELETE_KEY"
apply_stage 05-after-prune update-after-prune-001
apply_stage 05-after-prune unchanged-after-prune-001
```

Only one resource should remain. For AWS, deleting the VPC while retaining its subnet is rejected
before sending a delete. `--key` can be repeated; selected dependents are deleted first. Omitting a
resource from an apply file alone does not delete it. After pruning, keep using the reduced files;
reapplying the original two-resource declaration requests recreation of the removed resource.

Run a local CLI action in every profile. This creates a local private key; only its public digest may
be supplied as infrastructure intent. A 128-bit public digest is used to fit Google's label limit.

```sh
openssl genpkey -algorithm ED25519 -out "$CASE/local-private.pem"
chmod 600 "$CASE/local-private.pem"
openssl pkey -in "$CASE/local-private.pem" -pubout -out "$CASE/local-public.pem"
PUBLIC_DIGEST=$(openssl pkey -in "$CASE/local-private.pem" -pubout -outform DER \
  | openssl dgst -sha256 | awk '{print substr($NF,1,32)}')
build/omnicli iac-apply "$CASE/registry" "@$CASE/06-local.json" \
  --input "{\"public_digest\":\"$PUBLIC_DIGEST\"}" \
  --name "$COLLECTION" --state "$CASE/state" --run-id local-001
```

AWS and Storage use the public digest as ordinary tag/label intent. The network profiles retain the
key locally: network patch supports routing configuration, not arbitrary key metadata. This local
command is a caller-side preparation step, not a durable Omni exchange; there is no hidden shell execution.

Finally delete all remaining resources and repeat to confirm an empty deployment:

```sh
build/omnicli iac-destroy --registry "$CASE/registry" --name "$COLLECTION" \
  --state "$CASE/state" --run-id destroy-001 --params "$SCOPE"
build/omnicli iac-destroy --registry "$CASE/registry" --name "$COLLECTION" \
  --state "$CASE/state" --run-id destroy-check-001 --params "$SCOPE"
```

Check every output row: `failed` or `outstanding` is a failure even if the current CLI exit code is zero.
Keep the ledger and journal if anything is outstanding. Cloud teardown does not delete the local key;
remove that file yourself when finished.

## Independent live checks

AWS: use the printed VPC/subnet IDs with `aws ec2 describe-vpcs --region ap-southeast-2 --vpc-ids ID`
and `aws ec2 describe-subnets --region ap-southeast-2 --subnet-ids ID`. Check `resource-key` tags,
absence of the subnet after prune, and absence of both after final teardown.

Google networks:

```sh
gcloud compute networks describe "$PREFIX-main" --project "$PROJECT" \
  --format='json(name,id,routingConfig)'
gcloud compute networks describe "$PREFIX-secondary" --project "$PROJECT" \
  --format='json(name,id,routingConfig)'
```

Routing mode alternates `REGIONAL`, `GLOBAL`, `REGIONAL`, `GLOBAL`, then `REGIONAL` after prune.
The secondary network should be absent after prune; both should be absent after final deletion.

Storage:

```sh
gcloud storage buckets describe "gs://$PREFIX-main" --project "$PROJECT" --format=json
gcloud storage buckets describe "gs://$PREFIX-secondary" --project "$PROJECT" --format=json
```

Check label revisions, then the public digest on the surviving bucket. Keep these buckets empty;
the walkthrough never authorizes deleting object contents to force bucket deletion.

Provider behavior: [network patch](https://docs.cloud.google.com/compute/docs/reference/rest/v1/networks/patch),
[bucket labels](https://docs.cloud.google.com/storage/docs/using-bucket-labels).

## Automated black-box verification

```sh
go build -o build/omnicli ./cmd/omnicli
test/mock/.venv/bin/robot --outputdir test/robot/out/identity test/robot/identity_lifecycle.robot
```

`test/robot/run.sh` includes the suite in the normal CI run. OpenSSL is required. Core cases assert
API-visible state, stable identities, mutation-free reruns, parent-readiness ordering, retained-dependent
rejection, partial deletion, further updates, and complete cleanup. Additional cases execute the
same generated Google document shapes with mocked endpoints and bearer credentials. Poll intervals
are shortened only in those mock copies. None of these automated tests deploys live cloud resources.
