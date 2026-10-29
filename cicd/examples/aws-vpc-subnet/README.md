# AWS VPC and subnet

## Change the VPC Name tag declaratively

The VPC in `update-public-ip.json` and `update-private-ip.json` now declares:

```json
"desired": {
  "CidrBlock": "10.42.0.0/16",
  "Name": "omni-demo-vpc"
}
```

Change `desired.Name` and reapply the same file, collection and state directory. The
document's `Name` route calls `CreateTags` for the recorded VPC ID. Reapplying an unchanged
name reads the live tags and performs no tag mutation. External Name drift is repaired.
This route sets or overwrites only `Name`, preserving all other tags.
It does not implement tag deletion: keep a string Name in subsequent update inputs.

Regenerate an existing overlay after updating this example:

```sh
python3 cicd/examples/aws-vpc-subnet/prepare-update.py \
  --registry test/corpus/registry --out cicd/out/aws-update-registry --refresh
```

Then use the apply command below with a fresh run ID. The overlay uses the existing response
and request template DSLs to project the Name tag and encode the tagging request. The SDK
contains no special AWS tag-reconciliation logic. Mutable Name intent belongs in `desired.Name`.

## Live update of the existing subnet

After creating the VPC and subnet, apply `update-public-ip.json` to enable automatic
public IPv4 assignment on future instance launches in that subnet. It changes the existing
subnet through `ModifySubnetAttribute`; it does not launch an instance or add internet routing.
The API input is documented in [AWS ModifySubnetAttribute](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_ModifySubnetAttribute.html).

From the repository root, using the existing `demo` collection and ledger:

```sh
go build -o build/omnicli ./cmd/omnicli
python3 cicd/examples/aws-vpc-subnet/prepare-update.py \
  --registry test/corpus/registry --out cicd/out/aws-update-registry --refresh

build/omnicli iac-apply cicd/out/aws-update-registry \
  @cicd/examples/aws-vpc-subnet/update-public-ip.json \
  --aws-region ap-southeast-2 --name demo \
  --state "$PWD/cicd/out/aws-demo/state" --run-id public-ip-001
```

Run the same apply with `--run-id public-ip-check-002`: the attribute should already match,
so it must not issue another update. Resource identities must remain the same. Current result
rows use `status: applied` for successful no-op runs too; that label alone does not prove a mutation.

Verify directly if the AWS CLI is installed, using the subnet ID from your create output:

```sh
aws ec2 describe-subnets --region ap-southeast-2 \
  --subnet-ids YOUR_SUBNET_ID --query 'Subnets[0].MapPublicIpOnLaunch' --output text
```

To reverse the setting, apply `update-private-ip.json` with the same command and a fresh run ID.
Both versions retain the original VPC/subnet keys and CIDRs. Do not delete the ledger or change
collection names. These update files require the subnet to exist; they are not initial-create inputs.

The Python preparation command only writes an isolated provider-document overlay. It does not
call AWS. The overlay adds the update route and SQL-verb mapping missing from the corpus, and
exposes the read-back boolean under its EC2 Query input name `MapPublicIpOnLaunch.Value` so
diffing, sending and subsequent read-back agree. The source corpus is left untouched. Prepare
the overlay once; use a fresh output directory if regenerating it.


## Self, input and environment bindings

The files declare ordinary `resource-key` tags. Their values use:

```json
"TagSpecification.1.Tag.1.Value": {"from":"self", "attribute":"key"}
```

`write_once` explicitly names the tag key and resource type. No label or collection prefix is
injected. The provider document supplies ordinary tag create/read/update/delete mappings.
Removing the owned tag value from `desired` requests tag deletion on reapply.

`bindings.json` also demonstrates top-level caller inputs and an explicit environment reference:

```sh
AWS_REGION=ap-southeast-2 build/omnicli iac-apply cicd/out/aws-update-registry \
  @cicd/examples/aws-vpc-subnet/bindings.json \
  --input '{"vpc_label":"chosen-by-caller"}' \
  --name demo --state "$PWD/cicd/out/aws-demo/state" --run-id bindings-001
```

This is a real apply command. Refresh the registry first. Missing inputs or referenced environment
variables fail before mutation. The [binding contract](../../../docs/developer/resource_bindings.md)
lists available attributes and API inputs. The old correlation/debug-correlation options are rejected.

## Initial creation and teardown

`resources.json` explicitly declares a VPC and a subnet. The subnet receives the created
VPC identity through its inbox. Both resources explicitly declare their own tags.
This is the existing supported network foundation: it does not create public routing,
an instance, SSH keys, nginx or a load balancer.

From the repository root, supply your registry, region, collection and state directory:

```sh
go build -o build/omnicli ./cmd/omnicli
python3 cicd/examples/aws-vpc-subnet/prepare-update.py \
  --registry test/corpus/registry --out cicd/out/aws-update-registry --refresh
export REGISTRY="$PWD/cicd/out/aws-update-registry"
export REGION=YOUR_REGION
export COLLECTION=YOUR_COLLECTION
export STATE="$PWD/cicd/out/YOUR_DEPLOYMENT/state"

build/omnicli iac-apply "$REGISTRY" \
  @cicd/examples/aws-vpc-subnet/resources.json \
  --aws-region "$REGION" --name "$COLLECTION" --state "$STATE" --run-id create-001
```

This command creates real resources in the account belonging to your supplied AWS
credentials. Verify that account before running it. Scope is supplied explicitly;
credentials use the normal AWS credential configuration. Edit CIDRs before the first apply.
The provider determines available operations; this example does not promise CIDR replacement.
Keep the state directory so later runs can address the same deployment.

When ready to remove these two resources, use the same registry, region, collection and state:

```sh
build/omnicli iac-destroy --registry "$REGISTRY" \
  --aws-region "$REGION" --name "$COLLECTION" --state "$STATE" --run-id destroy-001
```

Inspect JSONL results for errors and outstanding resources. Teardown uses the ledger's
dependency order. No live AWS deployment is performed by creating these example files.
