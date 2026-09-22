## Running queries

This document assumes the same base as [the developer guide](/docs/developer/developer_guide.md).


```bash
go build -o build/omnicli ./cmd/omnicli

source cicd/vol/vendor-secrets/secrets.sh 
```

## Function queries

A `projections` entry declares a select list for one address. It **replaces** that address's row: a
field the list does not name is not emitted. Each column is exactly one of

- `{"field": "Name"}` — a column of the row, NULL when absent
- `{"literal": "/"}` — a constant
- `{"fn": "split_part", "args": [...]}` — a function over further expressions

Functions come in two shapes and the select list treats them the same way, because multiplicity is a
property of the return rather than of the call: `split_part` yields a value, `string_to_table` yields
rows and fans the input row out, carrying the scalar columns onto each. At most one row-producing
column per select — two would be a cross product, which is a join and is stated as one.

The projection runs **before** the row travels a β edge, so a join key may be a value a function
computed rather than one the document returned.

### Simple function projection example

`split_part` cuts each subnet's availability zone into the region and the zone letter.

```bash
_now="$(date +%s)" && ./build/omnicli doc-graph test/corpus/registry '{
  "addresses": ["stackql_unstable_aws.ec2.subnets"],
  "projections": [{
    "address": "stackql_unstable_aws.ec2.subnets",
    "select": [
      {"out": "subnet", "field": "SubnetId"},
      {"out": "vpc", "field": "VpcId"},
      {"out": "az", "field": "AvailabilityZone"},
      {"out": "zone", "fn": "split_part", "args": [{"field": "AvailabilityZone"}, {"literal": "-"}, {"literal": 3}]}
    ]
  }]
}' --aws-region "${_AWS_REGION}" --out "./cicd/out/simple-function-projection-${_now}.jsonl" --log "./cicd/out/simple-function-projection-${_now}.log"

```


### Simple table valued function projection example

`string_to_table` turns one subnet row into one row per octet of its CIDR prefix. `subnet` and `cidr`
are scalar columns and are carried onto every produced row.

```bash
_now="$(date +%s)" && ./build/omnicli doc-graph test/corpus/registry '{
  "addresses": ["stackql_unstable_aws.ec2.subnets"],
  "projections": [{
    "address": "stackql_unstable_aws.ec2.subnets",
    "select": [
      {"out": "subnet", "field": "SubnetId"},
      {"out": "cidr", "field": "CidrBlock"},
      {"out": "octet", "fn": "string_to_table", "args": [
        {"fn": "split_part", "args": [{"field": "CidrBlock"}, {"literal": "/"}, {"literal": 1}]},
        {"literal": "."}
      ]}
    ]
  }]
}' --aws-region "${_AWS_REGION}" --out "./cicd/out/simple-table-valued-function-projection-${_now}.jsonl" --log "./cicd/out/simple-table-valued-function-projection-${_now}.log"

```

### Simple join on function example

The join key is computed, not returned: `split_part` picks the first availability zone out of a
literal list, and each VPC's subnets are filtered by it. `VpcId` is selected explicitly because the
projection replaces the row and the edge still needs it.

```bash
_now="$(date +%s)" && ./build/omnicli doc-graph test/corpus/registry '{
  "addresses": ["stackql_unstable_aws.ec2.vpcs", "stackql_unstable_aws.ec2.subnets"],
  "projections": [{
    "address": "stackql_unstable_aws.ec2.vpcs",
    "select": [
      {"out": "VpcId", "field": "VpcId"},
      {"out": "az", "fn": "split_part", "args": [
        {"literal": "us-east-1a,us-east-1b"}, {"literal": ","}, {"literal": 1}
      ]}
    ]
  }],
  "wirings": [{
    "to": "stackql_unstable_aws.ec2.subnets",
    "inbound": [
      {"from": "stackql_unstable_aws.ec2.vpcs", "src": "VpcId", "as": "vpc_id"},
      {"from": "stackql_unstable_aws.ec2.vpcs", "src": "az", "as": "az"}
    ],
    "via_type": "golang_template_json_v0.1.0",
    "via": "{\"Filter.1.Name\":\"vpc-id\",\"Filter.1.Value.1\":\"{{ .vpc_id }}\",\"Filter.2.Name\":\"availability-zone\",\"Filter.2.Value.1\":\"{{ .az }}\"}",
    "provides": ["Filter.1.Name", "Filter.1.Value.1", "Filter.2.Name", "Filter.2.Value.1"]
  }]
}' --aws-region "${_AWS_REGION}" --out "./cicd/out/simple-scalar-function-join-${_now}.jsonl" --log "./cicd/out/simple-scalar-function-join-${_now}.log"

```

### Simple join on table valued function example

The same query with `string_to_table` in place of `split_part`: each VPC fans out into one row per
availability zone, and every one of those rows drives its own `DescribeSubnets` call. Two VPCs and
two zones is four calls, not two — the fan-out happens before the edge.

```bash
_now="$(date +%s)" && ./build/omnicli doc-graph test/corpus/registry '{
  "addresses": ["stackql_unstable_aws.ec2.vpcs", "stackql_unstable_aws.ec2.subnets"],
  "projections": [{
    "address": "stackql_unstable_aws.ec2.vpcs",
    "select": [
      {"out": "VpcId", "field": "VpcId"},
      {"out": "az", "fn": "string_to_table", "args": [
        {"literal": "us-east-1a,us-east-1b"}, {"literal": ","}
      ]}
    ]
  }],
  "wirings": [{
    "to": "stackql_unstable_aws.ec2.subnets",
    "inbound": [
      {"from": "stackql_unstable_aws.ec2.vpcs", "src": "VpcId", "as": "vpc_id"},
      {"from": "stackql_unstable_aws.ec2.vpcs", "src": "az", "as": "az"}
    ],
    "via_type": "golang_template_json_v0.1.0",
    "via": "{\"Filter.1.Name\":\"vpc-id\",\"Filter.1.Value.1\":\"{{ .vpc_id }}\",\"Filter.2.Name\":\"availability-zone\",\"Filter.2.Value.1\":\"{{ .az }}\"}",
    "provides": ["Filter.1.Name", "Filter.1.Value.1", "Filter.2.Name", "Filter.2.Value.1"]
  }]
}' --aws-region "${_AWS_REGION}" --out "./cicd/out/simple-table-valued-function-join-${_now}.jsonl" --log "./cicd/out/simple-table-valued-function-join-${_now}.log"

```
