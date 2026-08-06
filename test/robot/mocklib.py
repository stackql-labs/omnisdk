"""Robot keywords: wait for the mock, and compare JSONL output semantically."""

import json
import sys
import time
import urllib.request


def python_executable():
    """The interpreter running robot (the venv python) — use it to launch the mock so it has
    the installed deps, rather than whatever bare `python` is on PATH (fails in CI)."""
    return sys.executable


def wait_for_server(url, timeout=15):
    """Poll url until it answers or timeout (seconds) elapses."""
    deadline = time.time() + float(timeout)
    last = None
    while time.time() < deadline:
        try:
            urllib.request.urlopen(url, timeout=1)
            return
        except Exception as exc:  # noqa: BLE001 - any failure = not up yet
            last = exc
            time.sleep(0.2)
    raise AssertionError(f"server not up at {url}: {last}")


def wait_for_tcp(host, port, timeout=15):
    """Poll a TCP host:port until it accepts a connection (for the gRPC mock)."""
    import socket

    deadline = time.time() + float(timeout)
    last = None
    while time.time() < deadline:
        try:
            with socket.create_connection((host, int(port)), timeout=1):
                return
        except Exception as exc:  # noqa: BLE001 - any failure = not up yet
            last = exc
            time.sleep(0.2)
    raise AssertionError(f"tcp not up at {host}:{port}: {last}")


def write_gcp_service_account(path):
    """Generate a throwaway RSA service-account key + SA json at path (so the OAuth exchange can
    sign a JWT; the mock ignores the signature). Never a real credential — generated per run."""
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric import rsa

    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    pem = key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    ).decode()
    sa = {
        "type": "service_account",
        "project_id": "mock-project",
        "client_email": "mock@mock-project.iam.gserviceaccount.com",
        "private_key": pem,
        "token_uri": "https://oauth2.googleapis.com/token",
    }
    with open(path, "w") as fh:
        json.dump(sa, fh)


def read_json(path):
    """Parse a whole JSON file into a dict/list."""
    with open(path) as fh:
        return json.load(fh)


def read_json_line(path):
    """Parse the first non-empty JSONL line into a dict."""
    with open(path) as fh:
        for line in fh:
            line = line.strip()
            if line:
                return json.loads(line)
    raise AssertionError(f"no JSON line in {path}")


def _canon(path):
    rows = []
    with open(path) as fh:
        for line in fh:
            line = line.strip()
            if line:
                rows.append(json.dumps(json.loads(line), sort_keys=True))
    return sorted(rows)


def assert_jsonl_semantically_equal(actual, expected):
    """Assert two JSONL files hold the same set of objects (key order irrelevant)."""
    a, e = _canon(actual), _canon(expected)
    if a == e:
        return
    only_actual = sorted(set(a) - set(e))
    only_expected = sorted(set(e) - set(a))
    raise AssertionError(
        f"jsonl mismatch: {len(a)} actual vs {len(e)} expected rows\n"
        f"only-in-actual ({len(only_actual)}): {only_actual[:3]}\n"
        f"only-in-expected ({len(only_expected)}): {only_expected[:3]}"
    )
