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
  "nodes": [{"alias": "s", "address": "stackql_unstable_aws.ec2.subnets"}],
  "projections": [{
    "alias": "s",
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
  "nodes": [{"alias": "s", "address": "stackql_unstable_aws.ec2.subnets"}],
  "projections": [{
    "alias": "s",
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
  "nodes": [{"alias": "v", "address": "stackql_unstable_aws.ec2.vpcs"}, {"alias": "s", "address": "stackql_unstable_aws.ec2.subnets"}],
  "projections": [{
    "alias": "v",
    "select": [
      {"out": "VpcId", "field": "VpcId"},
      {"out": "az", "fn": "split_part", "args": [
        {"literal": "us-east-1a,us-east-1b"}, {"literal": ","}, {"literal": 1}
      ]}
    ]
  }],
  "wirings": [{
    "to": "s",
    "inbound": [
      {"from": "v", "src": "VpcId", "as": "vpc_id"},
      {"from": "v", "src": "az", "as": "az"}
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
  "nodes": [{"alias": "v", "address": "stackql_unstable_aws.ec2.vpcs"}, {"alias": "s", "address": "stackql_unstable_aws.ec2.subnets"}],
  "projections": [{
    "alias": "v",
    "select": [
      {"out": "VpcId", "field": "VpcId"},
      {"out": "az", "fn": "string_to_table", "args": [
        {"literal": "us-east-1a,us-east-1b"}, {"literal": ","}
      ]}
    ]
  }],
  "wirings": [{
    "to": "s",
    "inbound": [
      {"from": "v", "src": "VpcId", "as": "vpc_id"},
      {"from": "v", "src": "az", "as": "az"}
    ],
    "via_type": "golang_template_json_v0.1.0",
    "via": "{\"Filter.1.Name\":\"vpc-id\",\"Filter.1.Value.1\":\"{{ .vpc_id }}\",\"Filter.2.Name\":\"availability-zone\",\"Filter.2.Value.1\":\"{{ .az }}\"}",
    "provides": ["Filter.1.Name", "Filter.1.Value.1", "Filter.2.Name", "Filter.2.Value.1"]
  }]
}' --aws-region "${_AWS_REGION}" --out "./cicd/out/simple-table-valued-function-join-${_now}.jsonl" --log "./cicd/out/simple-table-valued-function-join-${_now}.log"

```

## Create, poll, then read

A create on Google Compute returns a long-running **operation**, not the thing it made. Reading the
result takes three steps: create, poll the operation until it is `DONE`, then get what it made. That
is one graph with three nodes, and every node's columns come out in the row as usual:

```
c: networks insert ──name→operation──▶ o: global_operations get (poll until DONE) ──targetLink→network──▶ n: networks get
```

Three things make it expressible:

- **`verb` and `body` on a node.** `c` runs the document's `insert` method, not a select. `body`
  names which of its params are request-body fields (`name`); the rest (`project`) are parameters.
  A mutating node is never retried or replayed.
- **`poll` on an override.** It re-requests the exchange until `status_path` reads `done`, waiting
  `interval` between attempts, at most `max_attempts` times. Every bound is required.
- **`patches` with a `doc_cache`.** The compute document declares the global-operation URL but no
  method for it, so `global_operations` cannot be read one at a time. A patch — an RFC 7386 merge
  patch on one service document, addressed `<provider>.<service>` — adds the `get` for this query
  only. Patched documents are written under `doc_cache.dir`, keyed by the patches, and reused by
  every query with the same ones; `"fresh": true` rebuilds them. Every other document, and every
  other caller, reads the registry unchanged.

`n`'s wiring cuts the network's name out of the operation's `targetLink`
(`https://www.googleapis.com/compute/v1/projects/<p>/global/networks/<name>`).

Auth values that travel on the row (the service account's signed assertion, the bearer token) are
dropped from results by default. `--show-credentials` keeps them, for a caller who needs them and
takes responsibility for the output; in the SDK, `Args.Redaction` takes any policy.

This **creates a network** in `${_GOOGLE_PROJECT_ID}`.

```bash
_now="$(date +%s)" && ./build/omnicli doc-graph test/corpus/registry '{
  "patches": [{
    "service": "stackql_unstable_google.compute",
    "merge": {"components": {"x-stackQL-resources": {"global_operations": {
      "methods": {"get": {
        "operation": {"$ref": "#/paths/~1projects~1{project}~1global~1operations~1{operation}/get"},
        "response": {"mediaType": "application/json", "openAPIDocKey": "200"}
      }},
      "sqlVerbs": {"select": [
        {"$ref": "#/components/x-stackQL-resources/global_operations/methods/aggregated_list"},
        {"$ref": "#/components/x-stackQL-resources/global_operations/methods/get"}
      ]}
    }}}}
  }],
  "doc_cache": {"dir": "./cicd/out/doc-cache"},
  "nodes": [
    {"alias": "c", "address": "stackql_unstable_google.compute.networks", "verb": "insert",
     "params": {"project": "'"${_GOOGLE_PROJECT_ID}"'", "name": "omnisdk-demo-'"${_now}"'"}, "body": ["name"]},
    {"alias": "o", "address": "stackql_unstable_google.compute.global_operations",
     "params": {"project": "'"${_GOOGLE_PROJECT_ID}"'"}},
    {"alias": "n", "address": "stackql_unstable_google.compute.networks",
     "params": {"project": "'"${_GOOGLE_PROJECT_ID}"'"}}
  ],
  "wirings": [
    {"to": "o", "inbound": [{"from": "c", "src": "name", "as": "operation"}]},
    {"to": "n", "inbound": [{"from": "o", "src": "targetLink", "as": "link"}],
     "via_type": "golang_template_json_v0.1.0",
     "via": "{\"network\":\"{{ index (split \"/\" .link) 9 }}\"}",
     "provides": ["network"]}
  ],
  "overrides": [{
    "address": "stackql_unstable_google.compute.global_operations",
    "poll": {"status_path": "status", "done": "DONE", "interval": "2s", "max_attempts": 60}
  }]
}' --out "./cicd/out/create-poll-read-${_now}.jsonl" --log "./cicd/out/create-poll-read-${_now}.log"

```
