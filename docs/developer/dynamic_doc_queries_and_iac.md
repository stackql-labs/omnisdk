
# Dynamic Queries and IAC

### Calling it from Go

The CLI is a thin consumer of the facade; a client such as stackql imports `pkg/omnisdk` and reaches
the same functions. Both paths return the same `Plan`/`Rows` a single-method query returns, so a
consumer iterates one cursor shape whether it is reading or provisioning.

**Auth and document location behave exactly as elsewhere.** The provider document declares the scheme
— `aws_signing_v4`, `service_account`, `oauth2` — and the credential comes from `Args.Auth`, falling
back to the canonical `AWS_*`, `AZURE_*` and `GOOGLE_*` variables. Only the credential a document
actually declares is required, so a run touching AWS alone does not fail because a Google key
elsewhere is stale; a credential that IS present but unusable says so rather than reporting as
absent. The registry root is a parameter rather than configuration, and is required — which document
set a run resolves against is scope, and scope is never inferred.

**1. A dynamic query across provider documents** — the `doc-graph` equivalent:

```go
const vpcs, subnets = "stackql_unstable_aws.ec2.vpcs", "stackql_unstable_aws.ec2.subnets"

g, err := omnisdk.NewGraph(
    []string{vpcs, subnets},
    []omnisdk.Wiring{omnisdk.NewWiring(
        subnets,                                                        // the consumer
        []omnisdk.Inbound{omnisdk.NewInbound(vpcs, "VpcId", "vpc_id")}, // β: src → inbox label
        gotemplate.TypeJSON1,                                           // T_in, or "" for identity
        `{"Filter.1.Name":"vpc-id","Filter.1.Value.1":"{{ .vpc_id }}"}`,
        "Filter.1.Name", "Filter.1.Value.1",                            // what T_in provides
    )},
    // Corrections to what a document says about its response, where it is wrong for this engine:
    // omnisdk.NewOverride(addr, "$.items", "", "", ""),
)
if err != nil {
    return err
}

pl, err := omnisdk.NewGraphQuery(registryRoot, g, omnisdk.Args{
    Auth:   auth,                                        // nil falls back to the env
    Params: map[string]string{"region": "us-east-1"},    // scope
})
rows, err := pl.Open(ctx)
defer rows.Close()
for rows.Next() {
    row := rows.Row() // {"VpcId":…, "SubnetId":…, "CidrBlock":…}
}
return rows.Err()
```

**2. An idempotent IaC run** — the `iac-apply` equivalent:

```go
res := []omnisdk.ManagedResource{
    omnisdk.NewResource(
        "aws/ec2/vpc",                   // key within the collection
        "aws", "ec2.vpcs",               // registry provider, document address
        []byte(`{"CidrBlock":"10.42.0.0/16"}`),
        map[string]string{
            "TagSpecification.1.ResourceType": "vpc",
            "TagSpecification.1.Tag.1.Key":    "omnisdk:key",
        },
        nil, "", "",                     // inbound, T_in type, T_in program
        "line_items.VpcId",              // where the minted id sits in the projected response
        "VpcId",                         // the parameter addressing an existing object
        "TagSpecification.1.Tag.1.Value", // the parameter taking the correlation stamp
    ),
    omnisdk.NewResource(
        "aws/ec2/subnet", "aws", "ec2.subnets",
        []byte(`{"CidrBlock":"10.42.1.0/24"}`), nil,
        []omnisdk.Arrival{{From: "aws/ec2/vpc", As: "VpcId"}}, "", "",
        "line_items.SubnetId", "SubnetId", "",
    ),
}

pl, err := omnisdk.Converge(registryRoot, "scratch", stateDir, "" /* runID: timestamp */, res,
    omnisdk.Args{Auth: auth, Params: map[string]string{"region": "us-east-1"}})
if err != nil {
    return err
}
rows, err := pl.Open(ctx)   // the run happens here
```

Rows report `{"key":…, "identity":…, "status":…}`, with a final row carrying `error`,
`compensated` and `outstanding` when a run failed. A non-empty `outstanding` means the run is
*partially* compensated — something it created is still there and could not be removed.

Precanned deployments are reachable the same way, and are only a way of building that slice:

```go
bp, ok := omnisdk.BlueprintFor("aws-vpc-subnet")
res, err := bp.Resources(map[string]string{
    "region": "us-east-1", "vpc_cidr": "10.42.0.0/16", "subnet_cidr": "10.42.1.0/24",
})
```

`omnisdk.Blueprints()` lists them with the inputs each declares, which is what `iac-handles` prints.