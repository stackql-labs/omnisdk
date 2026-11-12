"""Alternative Google input requirements through a real CLI and loopback mock."""

import copy
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import subprocess
import threading


class AlternativeInputsLibrary:
    ROBOT_LIBRARY_SCOPE = "SUITE"

    def prepare_alternative_inputs(self, binary):
        self.binary = str(Path(binary).resolve())
        self.fixture = Path(__file__).resolve().parents[2] / "cicd/examples/alternative-input-requirements"
        self.spec = json.loads((self.fixture / "bulk-insert.json").read_text())
        self.calls, self.lock = [], threading.Lock()
        owner = self

        class API(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
                with owner.lock:
                    owner.calls.append({"method": self.command, "path": self.path, "body": body})
                valid = (
                    self.path == "/compute/v1/projects/mock-project/zones/us-central1-a/instances/bulkInsert"
                    and str(body.get("count")) == "2"
                    and (body.get("sourceInstanceTemplate") is not None
                         or body.get("instanceProperties") is not None)
                )
                payload = json.dumps({"name": "bulk-operation-1", "status": "DONE"}
                                     if valid else {"error": "invalid bulkInsert inputs"}).encode()
                self.send_response(200 if valid else 400)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), API)
        self.worker = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.worker.start()
        self.endpoint = f"http://127.0.0.1:{self.server.server_port}"

    def run_bulk_insert_case(self, scenario):
        spec = copy.deepcopy(self.spec)
        node = spec["nodes"][0]
        body = node["body"]
        if scenario == "template":
            del body["instanceProperties"]
        elif scenario == "properties":
            del body["sourceInstanceTemplate"]
        elif scenario == "neither":
            del body["instanceProperties"]
            del body["sourceInstanceTemplate"]
        elif scenario == "missing-project":
            del node["params"]["project"]
        elif scenario == "missing-count":
            del body["count"]
        elif scenario != "both":
            raise AssertionError(f"unknown scenario: {scenario}")
        with self.lock:
            start = len(self.calls)
        result = subprocess.run(
            [self.binary, "doc-graph", str(self.fixture / "registry"), json.dumps(spec),
             "--endpoint", self.endpoint], capture_output=True, text=True, timeout=15,
        )
        with self.lock:
            calls = self.calls[start:]
        accepted = scenario in {"template", "properties", "both"}
        if not accepted:
            assert result.returncode != 0, f"invalid input accepted: {result.stdout}"
            assert not calls, f"invalid input sent a mutation: {calls}"
            expected = "admission" if scenario == "neither" else "required inbox input"
            assert expected in result.stderr.lower(), result.stderr
            return
        assert result.returncode == 0, result.stderr
        assert len(calls) == 1, f"expected one create: {calls}"
        assert calls[0]["body"] == body, f"request changed: {calls[0]}"
        rows = [json.loads(line) for line in result.stdout.splitlines() if line.strip()]
        assert rows and any("DONE" in json.dumps(row) for row in rows), result.stdout

    def stop_alternative_inputs(self):
        if hasattr(self, "server"):
            self.server.shutdown()
            self.server.server_close()
            self.worker.join(timeout=5)
