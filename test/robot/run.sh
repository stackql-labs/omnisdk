#!/usr/bin/env bash
# Repeatable, self-initialising robot run. Idempotent: creates the venv if missing, installs
# deps, builds omnicli, then runs the suite. Invoke from anywhere:  test/robot/run.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

VENV="test/mock/.venv"
PY="${PYTHON:-python3}"

go build -o build/omnicli ./cmd/omnicli

[ -d "$VENV" ] || "$PY" -m venv "$VENV"
"$VENV/bin/pip" install -q --upgrade pip
"$VENV/bin/pip" install -q -r test/mock/requirements.txt

exec "$VENV/bin/robot" --outputdir test/robot/out test/robot/omnicli.robot
