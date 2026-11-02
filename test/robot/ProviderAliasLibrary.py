"""Account bindings through the real CLI with observable credential and scope selection."""
import copy
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse


class ProviderAliasLibrary:
    ROBOT_LIBRARY_SCOPE = "SUITE"

    def prepare_provider_aliases(self, binary):
        self.binary = str(Path(binary).resolve())
        self.root = Path(tempfile.mkdtemp(prefix="omni-alias-robot-"))
        self.calls, self.lock = [], threading.Lock()
        owner = self

        class API(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                token = self.headers.get("Authorization")
                region = parse_qs(urlparse(self.path).query).get("region", [""])[0]
                with owner.lock:
                    owner.calls.append((token, region))
                payload = json.dumps([{"id": "east-account" if token == "Bearer east-token" else "west-account"}]).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), API)
        self.worker = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.worker.start()
        fixture = Path(__file__).resolve().parents[2] / "cicd/examples/identity-lifecycle/mock-registry"
        provider = json.loads((fixture / "provider.json").read_text())
        provider["config"]["auth"] = {"type": "bearer"}
        doc = json.loads((fixture / "things.json").read_text())
        # No identity is supplied: this list endpoint returns the visible account.
        doc["paths"]["/widgets"]["get"]["parameters"] = [{"name": "region", "in": "query", "schema": {"type": "string"}}]
        root = self.root / "registry/widgets/v00.00.00000"
        (root / "services").mkdir(parents=True)
        (root / "provider.yaml").write_text(json.dumps(provider))
        (root / "services/things.yaml").write_text(json.dumps(doc))
        self.spec = {"nodes": [{"alias": "east", "address": "east.things.widgets"}, {"alias": "west", "address": "west.things.widgets"}],
                     "args": {"auth_by_provider": {"widgets": {"type": "bearer", "credentials": "wrong-global-token"}},
                              "provider_aliases": {name: {"provider": "widgets", "tenancy": name + "-account",
                                                          "auth": {"type": "bearer", "credentials": name + "-token"},
                                                          "params": {"region": name}, "tuning": {"max_concurrent": 1}}
                                                   for name in ["east", "west"]}}}

    def run_binding_spec(self, spec, valid):
        with self.lock:
            before = len(self.calls)
        result = subprocess.run([self.binary, "doc-graph", str(self.root / "registry"), json.dumps(spec),
                                 "--endpoint", f"http://127.0.0.1:{self.server.server_port}"], capture_output=True, text=True, timeout=30)
        if valid:
            assert result.returncode == 0, result.stderr
            with self.lock:
                assert set(self.calls[before:]) == {("Bearer east-token", "east"), ("Bearer west-token", "west")}, self.calls
            assert "east-token" not in result.stdout and "west-token" not in result.stdout, "credentials leaked in query output"
        else:
            assert result.returncode != 0, result.stdout
            with self.lock:
                assert len(self.calls) == before, "invalid binding reached the provider"

    def aliases_use_explicit_credentials_and_regions(self):
        self.run_binding_spec(self.spec, True)

    def missing_alias_auth_does_not_fall_back(self):
        spec = copy.deepcopy(self.spec)
        del spec["args"]["provider_aliases"]["east"]["auth"]
        self.run_binding_spec(spec, False)

    def same_tenancy_conflicting_limits_are_rejected(self):
        spec = copy.deepcopy(self.spec)
        spec["args"]["provider_aliases"]["west"]["tenancy"] = "east-account"
        spec["args"]["provider_aliases"]["west"]["tuning"]["max_concurrent"] = 2
        self.run_binding_spec(spec, False)

    def unresolved_secret_binding_is_rejected(self):
        spec = copy.deepcopy(self.spec)
        del spec["args"]["provider_aliases"]["east"]["auth"]
        spec["args"]["provider_aliases"]["east"]["auth_secret"] = "must-resolve-in-session"
        self.run_binding_spec(spec, False)

    def stop_provider_aliases(self):
        if hasattr(self, "server"):
            self.server.shutdown()
            self.server.server_close()
            self.worker.join(timeout=5)
        if hasattr(self, "root"):
            shutil.rmtree(self.root)
