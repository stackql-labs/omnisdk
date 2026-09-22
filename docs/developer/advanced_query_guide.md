
## Running queries

This document assumes the same base as [the developer guide](/docs/developer/developer_guide.md).


```bash
go build -o build/omnicli ./cmd/omnicli

source cicd/vol/vendor-secrets/secrets.sh 

```

## Function queries

### Simple function projection example

```bash
_now="$(date +%s)" && ./build/omnicli ... --out "./cicd/out/simple-function-projection-${_now}.jsonl" --log "./cicd/out/simple-function-projection-${_now}.log"


```


### Simple table valued function projection example

```bash
_now="$(date +%s)" && ./build/omnicli ... --out "./cicd/out/simple-table-valued-function-projection-${_now}.jsonl" --log "./cicd/out/simple-table-valued-function-projection-${_now}.log"


```

### Simple join on function example

```bash
_now="$(date +%s)" && ./build/omnicli ... --out "./cicd/out/simple-scalar-function-join-${_now}.jsonl" --log "./cicd/out/simple-scalar-function-join-${_now}.log"


```

### Simple join on table valued function example

```bash
_now="$(date +%s)" && ./build/omnicli ... --out "./cicd/out/simple-table-valued-function-join-${_now}.jsonl" --log "./cicd/out/simple-table-valued-function-join-${_now}.log"


```
