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
