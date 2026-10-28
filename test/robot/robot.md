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
