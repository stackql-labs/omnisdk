#!/usr/bin/env python3
"""Run the declared IaC lifecycle against a local API; no cloud credentials needed."""
import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import subprocess
import threading


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path,
                        help="fresh directory for this deployment's ledger and transcripts")
    options = parser.parse_args()
    binary = options.binary.resolve()
    output = options.out.resolve()
    output.mkdir(parents=True, exist_ok=False)
    example = Path(__file__).resolve().parent
    registry = str(example / "registry")
    state = str(output / "state")
    collection = "routing"
    items, calls = {}, []
    lock = threading.Lock()

    class API(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def handle_request(self):
            from urllib.parse import parse_qs, urlparse
            url = urlparse(self.path)
            code, result = 200, None
            with lock:
                calls.append({"method": self.command, "path": self.path})
                if self.command == "GET" and url.path == "/widgets":
                    identity = parse_qs(url.query).get("id", [""])[0]
                    result = [items[identity]] if identity in items else []
                elif self.command == "POST" and url.path == "/widgets":
                    body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                    body["id"] = "w-1"
                    items["w-1"] = body
                    result = body
                elif url.path.startswith("/widgets/") and url.path[9:] in items:
                    identity = url.path[9:]
                    if self.command == "PATCH":
                        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                        items[identity].update(body)
                        result = items[identity]
                    elif self.command == "DELETE":
                        del items[identity]
                        code = 204
                    else:
                        code, result = 405, {"error": "unsupported method"}
                else:
                    code, result = 404, {"error": "not found"}
                payload = b"" if code == 204 else json.dumps(result).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        do_GET = do_POST = do_PATCH = do_DELETE = handle_request

    server = ThreadingHTTPServer(("127.0.0.1", 0), API)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    endpoint = f"http://127.0.0.1:{server.server_port}"

    def run(label, command, expected_methods, rejected=False):
        with lock:
            calls.clear()
        process = subprocess.run([str(binary), *command], text=True, capture_output=True,
                                 timeout=90, check=False)
        (output / f"{label}.jsonl").write_text(process.stdout)
        (output / f"{label}.stderr").write_text(process.stderr)
        with lock:
            observed = list(calls)
        (output / f"{label}.calls.json").write_text(json.dumps(observed, indent=2) + "\n")
        methods = [call["method"] for call in observed]
        if methods != expected_methods:
            raise RuntimeError(f"{label}: calls {methods}, expected {expected_methods}; "
                               f"stdout={process.stdout!r}; stderr={process.stderr!r}")
        if rejected:
            if "no declared update route" not in process.stdout + process.stderr:
                raise RuntimeError(f"{label}: expected explicit routing rejection")
        else:
            if process.returncode:
                raise RuntimeError(f"{label}: {process.stderr}")
            for line in process.stdout.splitlines():
                if json.loads(line).get("error"):
                    raise RuntimeError(f"{label}: {line}")
        print(f"PASS {label}: {methods}")

    def apply(label, spec, methods, rejected=False):
        run(label, ["iac-apply", registry, "@" + str(example / spec),
                    "--name", collection, "--state", state, "--run-id", label,
                    "--endpoint", endpoint], methods, rejected)

    try:
        apply("01-create", "01-create.json", ["POST"])
        apply("02-unchanged", "01-create.json", ["GET"])
        apply("03-update", "02-update.json", ["GET", "PATCH"])
        apply("04-rejected-update", "03-rejected-update.json", ["GET"], rejected=True)
        with lock:
            if items != {"w-1": {"id": "w-1", "size": "S", "color": "blue"}}:
                raise RuntimeError(f"unexpected state after rejected update: {items}")
        run("05-destroy", ["iac-destroy", "--registry", registry, "--name", collection,
                           "--state", state, "--run-id", "05-destroy", "--endpoint", endpoint],
            ["DELETE"])
        with lock:
            if items:
                raise RuntimeError("destroy left managed objects behind")
        print(f"Ledger and transcripts: {output}")
    finally:
        server.shutdown()
        server.server_close()
        worker.join()


if __name__ == "__main__":
    main()
