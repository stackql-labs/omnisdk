
# Dynamic Queries and IAC

### Calling it from Go

The CLI is a thin consumer of the facade; a client such as stackql imports `pkg/omnisdk` and reaches
the same functions. Both paths return the same `Plan`/`Rows` a single-method query returns, so a
consumer iterates one cursor shape whether it is reading or provisioning.

**Auth and document location behave exactly as elsewhere.** The provider document declares the scheme
— `aws_signing_v4`, `service_account`, `oauth2` — and the credential comes from the provider's entry in `Args.AuthByProvider`, falling
back to the canonical `AWS_*`, `AZURE_*` and `GOOGLE_*` variables. Only the credential a document
actually declares is required, so a run touching AWS alone does not fail because a Google key
elsewhere is stale; a credential that IS present but unusable says so rather than reporting as
absent. The registry root is a parameter rather than configuration, and is required — which document
set a run resolves against is scope, and scope is never inferred.

**1. A dynamic query across provider documents** — the `doc-graph` equivalent:

```go
const vpcs, subnets = "stackql_unstable_aws.ec2.vpcs", "stackql_unstable_aws.ec2.subnets"

g, err := omnisdk.NewGraph(
    []omnisdk.Node{                              // one per table reference, keyed by alias
        omnisdk.NewNode("v", vpcs, nil),
        omnisdk.NewNode("s", subnets, nil),      // per-reference params override Args.Params
    },
    []omnisdk.Wiring{omnisdk.NewWiring(
        "s",                                                             // the consumer
        []omnisdk.Inbound{omnisdk.NewInbound("v", "VpcId", "vpc_id")},  // β: src → inbox label
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

pl, err := omnisdk.NewGraphSelectQuery(registryRoot, g, omnisdk.Args{
    AuthByProvider: map[string]*omnisdk.Auth{"aws": awsAuth},    // per provider; absent falls back to the env
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
    omnisdk.Args{AuthByProvider: map[string]*omnisdk.Auth{"aws": awsAuth}, Params: map[string]string{"region": "us-east-1"}})
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

**3. SQL, already parsed** — what stackql sends. Describe each table, resolve, run:

```go
q, err := query.New(
    []query.Join{
        query.NewJoin(query.NewResource("u", "aws.iam.users"), query.Base),
        query.NewJoin(query.NewResource("k", "aws.iam.access_keys"), query.Left,
            query.NewEq(query.NewColumn("k", "UserName"), query.NewColumn("u", "UserName"))),
    },
    []query.Predicate{query.NewEq(query.NewColumn("", "region"), query.NewLiteral("us-east-1"))},
    []query.Output{query.NewOutput("UserName", query.NewColumn("u", "UserName"))},
)
users, _ := omnisdk.DescribeTable(registryRoot, "stackql_unstable_aws.iam.users")
keys, _ := omnisdk.DescribeTable(registryRoot, "stackql_unstable_aws.iam.access_keys")
res, err := omnisdk.Resolve(q, map[string]omnisdk.Table{"u": users, "k": keys})
pl, err := omnisdk.NewGraphSelectQuery(registryRoot, res.Graph(), omnisdk.Args{Params: res.Params()})
```

Join forms: `Inner` and `Left` send an `ON` value a table's methods take as a request per row;
`Listed` never does — the table is listed once and matched on its rows; `Cross` is every pair.

**4. UNION ALL** — several plans, one stream, one `Limit`; legs line up by column name:

```go
pl, err := omnisdk.UnionAll(awsPlan, googlePlan, azurePlan)
```

**5. Cycles** — a node wired to itself, bounded by a termination proved well-founded at planning:

```go
g, _ := omnisdk.NewGraph(
    []omnisdk.Node{omnisdk.NewNode("f", folders, map[string]string{"parent": "root"})},
    []omnisdk.Wiring{omnisdk.NewWiring("f", []omnisdk.Inbound{omnisdk.NewInbound("f", "id", "parent")}, "", "")},
)
g, err := omnisdk.WithTermination(g, "f", omnisdk.Rounds(10))
```

**6. Branches** — the first arm whose condition holds, once per row; gates say what runs on it:

```go
br, _ := omnisdk.NewBranch("pick",
    omnisdk.NewArm("deep", query.NewEq(query.NewColumn("top", "id"), query.NewLiteral("a"))),
    omnisdk.Otherwise("shallow"))
g, err := omnisdk.WithBranch(g, br, omnisdk.NewGate("pick", "deep", "sub"))
```

**7. IaC as a graph** — recall, read, diff, branch, then the mutation the arm calls for:

```go
nodes := []omnisdk.Node{
    omnisdk.NewRecallNode("r", "w1"),                                   // what the ledger knows
    omnisdk.NewOuterNode(omnisdk.NewNode("live", widgets, nil), nil),   // read by identity
    omnisdk.NewDiffNode("d", "w1", "live", "id", map[string]any{"size": "L"}),
    omnisdk.NewMutationNode("create", widgets, "insert", nil, nil, map[string]any{"size": nil}),
    omnisdk.NewMutationNode("update", widgets, "update", nil, nil, map[string]any{"size": nil}),
}
// Wire r.identity → live.id, d.size → create/update.size, d.identity → update.id; branch on
// d.status (DiffAbsent → create, DiffDrift → update); then:
pl, err := omnisdk.ConvergeGraph(registryRoot, "demo", stateDir, "", g,
    []omnisdk.Managed{omnisdk.NewManaged("w1", []string{"create", "update"}, "id", "id")}, args)
```

Every mutation node belongs to a `Managed`. A failed run deletes what it created and reports what it
left in place as `completed`. Gaps: [iac-gaps.md](iac-gaps.md).
