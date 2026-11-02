"""Document endpoint selectors through the real CLI and loopback OAuth providers."""
import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class EndpointSelectorLibrary:
    ROBOT_LIBRARY_SCOPE = "SUITE"

    def prepare_endpoint_selectors(self, binary):
        self.binary = str(Path(binary).resolve())
        self.root = Path(tempfile.mkdtemp(prefix="omni-endpoint-selectors-"))
        self.calls, self.lock, self.servers = [], threading.Lock(), []
        owner = self

        def start(label):
            class API(BaseHTTPRequestHandler):
                def log_message(self, *_):
                    pass

                def do_POST(self):
                    self.rfile.read(int(self.headers.get("Content-Length", "0")))
                    self.reply({"access_token": "selected-token", "token_type": "Bearer", "expires_in": 3600})

                def do_GET(self):
                    if self.headers.get("Authorization") != "Bearer selected-token":
                        self.send_error(401)
                        return
                    self.reply([{"id": label}])

                def reply(self, data):
                    with owner.lock:
                        owner.calls.append((label, self.command, self.path))
                    payload = json.dumps(data).encode()
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)

            server = ThreadingHTTPServer(("127.0.0.1", 0), API)
            worker = threading.Thread(target=server.serve_forever, daemon=True)
            worker.start()
            self.servers.append((server, worker))
            return f"127.0.0.1:{server.server_port}"

        self.hosts = {label: start(label) for label in ["aws", "azure"]}
        variable = {"default": "documentation-only.invalid", "x-stackQL-envVar": "OMNI_SELECTOR_DIRECT",
                    "x-stackQL-envVarSelector": {"envVar": "OMNI_SELECTOR_CLOUD", "default": "aws", "values": self.hosts}}
        fixture = Path(__file__).resolve().parents[2] / "cicd/examples/identity-lifecycle/mock-registry"
        provider = json.loads((fixture / "provider.json").read_text())
        provider["config"]["auth"] = {"type": "oauth2", "grant_type": "client_credentials",
                                        "token_url": "http://{account_host}/accounts/{{ .__env__OMNI_SELECTOR_ACCOUNT }}/token",
                                        "variables": {"account_host": variable}}
        doc = json.loads((fixture / "things.json").read_text())
        doc["servers"] = [{"url": "http://{account_host}", "variables": {"account_host": variable}}]
        doc["paths"]["/widgets"]["get"]["parameters"] = []
        version = self.root / "registry/widgets/v00.00.00000"
        (version / "services").mkdir(parents=True)
        (version / "provider.yaml").write_text(json.dumps(provider))
        (version / "services/things.yaml").write_text(json.dumps(doc))
        self.spec = {"nodes": [{"alias": "items", "address": "stackql_unstable_widgets.things.widgets",
                                "params": {"account_host": self.hosts["aws"]}}],
                     "args": {"auth_by_provider": {"widgets": {"type": "client_credentials", "client_id": "test-client", "client_secret": "test-secret"}}}}

    def check_endpoint_selector(self, scenario):
        spec = copy.deepcopy(self.spec)
        env = dict(os.environ, OMNI_SELECTOR_CLOUD="", OMNI_SELECTOR_DIRECT="", OMNI_SELECTOR_ACCOUNT="account-1")
        api, token, token_path = "aws", "aws", "/accounts/account-1/token"
        if scenario == "selector":
            env["OMNI_SELECTOR_CLOUD"] = " azure "
            api, token = "azure", "azure"
        if scenario == "direct":
            env["OMNI_SELECTOR_CLOUD"] = "invalid"
            env["OMNI_SELECTOR_DIRECT"] = " " + self.hosts["azure"] + "/// "
            api, token = "azure", "azure"
        if scenario == "runtime":
            env["OMNI_SELECTOR_CLOUD"] = "invalid"
            token, token_path = "azure", "/custom-token"
            spec["args"]["auth_by_provider"]["widgets"]["token_url"] = "http://" + self.hosts["azure"] + token_path
        spec["nodes"][0]["params"]["account_host"] = self.hosts[api]
        if scenario == "unknown":
            env["OMNI_SELECTOR_CLOUD"] = "typo"
        if scenario == "unbound":
            spec["nodes"][0]["params"] = {}
        with self.lock:
            before = len(self.calls)
        result = subprocess.run([self.binary, "doc-graph", str(self.root / "registry"), json.dumps(spec)],
                                env=env, capture_output=True, text=True, timeout=30)
        with self.lock:
            calls = self.calls[before:]
        if scenario in ["unknown", "unbound"]:
            assert result.returncode != 0, result.stdout
            assert not calls, f"invalid configuration sent requests: {calls}"
            if scenario == "unknown":
                assert "OMNI_SELECTOR_CLOUD" in result.stderr and "typo" in result.stderr, result.stderr
        else:
            assert result.returncode == 0, result.stderr
            assert calls == [(token, "POST", token_path), (api, "GET", "/widgets")], calls
            assert "test-secret" not in result.stdout, "credential leaked"

    def stop_endpoint_selectors(self):
        for server, worker in self.servers:
            server.shutdown()
            server.server_close()
            worker.join(timeout=5)
        shutil.rmtree(self.root)
