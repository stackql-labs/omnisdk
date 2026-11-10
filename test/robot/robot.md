# Mock S3 robot test

Runs `omnicli encryption` against a Flask mock (path-style S3) and asserts the JSONL matches
captured collateral.

From the repository root (self-initialising, idempotent — builds, makes the venv, runs):

```bash
./test/robot/run.sh
```

CI runs the same script. Robot report + process stdout/stderr land in `test/robot/out/`.

- `test/robot/` — `omnicli.robot` + `mocklib.py` (the test + keywords).
- `test/mock/app.py` — mock: `GET /` (ListBuckets, jinja) and `GET /<bucket>?encryption`.
- `test/mock/collateral/` — `buckets.json` + `encryption/<bucket>.xml` (anonymized).
- `test/mock/expected/encryption.jsonl` — expected output.

## Caller access management

`access.robot` exercises the actual CLI without cloud mocks: bearer and local OS identity,
directory groups, empty membership, missing sessions, invalid credentials, unresolved identities,
and denied command permissions. POSIX OS-identity cases skip on platforms without an effective UID.
Configurations are generated in a temporary directory and removed at suite teardown.

The standard `./test/robot/run.sh` discovers both `access.robot` and `omnicli.robot`.
To run only access management after building the CLI:

```bash
go build -o build/omnicli ./cmd/omnicli
test/mock/.venv/bin/robot --outputdir test/robot/out test/robot/access.robot
```

## JWT caller policy

`jwt_access.robot` invokes the compiled CLI with signed Ed25519/RSA JWTs and a local API. It checks
reader SELECT success, mutation/EXEC/IaC denial, token validity, directory membership and provider
permission boundaries. Test keys are generated per suite; tokens are passed through environment
variables and are not printed. This tests an in-memory directory; the RDBMS adapter is host-owned.

```sh
go build -o build/omnicli ./cmd/omnicli
test/mock/.venv/bin/robot --outputdir test/robot/out/jwt test/robot/jwt_access.robot
```

## Alternative input requirements

`alternative_inputs.robot` exercises Google-shaped `instances.bulkInsert` through the real CLI and
an isolated loopback API. Template only, properties only, and both succeed; neither alternative or
missing common inputs must fail before a mutation is sent. It requires no cloud credentials.

```sh
go build -o build/omnicli ./cmd/omnicli
test/mock/.venv/bin/robot --outputdir test/robot/out/alternative-inputs test/robot/alternative_inputs.robot
```

See the [canonical example](../../cicd/examples/alternative-input-requirements/README.md).

## AWS nginx lifecycle

`aws_nginx.robot` applies an EC2 document merge, provisions a VPC/two subnets/routing/SSH key/security
group/nginx VM against a strict local API, then verifies stop, termination and ledger-ordered cleanup.
It runs the static `resources.json`/`stopped.json` specs and EC2 merge through generic `omnicli` commands.
It needs the AWS corpus and `build/omnicli`, built automatically by `run.sh`.
See the [live-run and mock instructions](../../cicd/examples/aws-nginx/README.md).
