"""AWS execution-auth acceptance tests against the discrete Flask STS fixture."""
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading

from werkzeug.serving import make_server

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "mock"))
from sts import STSFixture, create_app


class STSRoleLibrary:
    def prepare_sts(self, binary):
        self.binary = str(Path(binary).resolve())
        self.root = Path(tempfile.mkdtemp(prefix="omni-sts-"))
        self.state = STSFixture()
        self.server = make_server("127.0.0.1", 0, create_app(self.state), threaded=True)
        self.worker = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.worker.start()
        self.endpoint = f"http://127.0.0.1:{self.server.server_port}"
        version = self.root / "registry/awsfixture/v00.00.00000"
        (version / "services").mkdir(parents=True)
        provider = {"name": "awsfixture", "id": "awsfixture", "version": "v00.00.00000", "config": {"auth": {"type": "aws_signing_v4"}},
                    "providerServices": {"iam": {"service": {"$ref": "awsfixture/v00.00.00000/services/iam.yaml"}}}}
        doc = {"openapi": "3.0.0", "info": {"title": "IAM fixture", "version": "1", "x-serviceName": "iam"},
               "servers": [{"url": "https://iam.{region}.example", "variables": {"region": {"default": "ap-southeast-2"}}}],
               "paths": {"/users": {"get": {"operationId": "ListUsers", "parameters": [{"name": "previous", "in": "query", "schema": {"type": "string"}}],
                   "responses": {"200": {"content": {"application/json": {"schema": {"type": "array", "items": {"type": "object", "properties": {"id": {"type": "string"}}}}}}}}}}},
               "components": {"x-stackQL-resources": {"users": {"methods": {"list": {"operation": {"$ref": "#/paths/~1users/get"}, "response": {"mediaType": "application/json"}}},
                   "sqlVerbs": {"select": [{"$ref": "#/components/x-stackQL-resources/users/methods/list"}]}}}}}
        (version / "provider.yaml").write_text(json.dumps(provider))
        (version / "services/iam.yaml").write_text(json.dumps(doc))
        self.auth = {"type": "aws_assume_role", "assume_role": {"role_arn": self.state.role, "external_id": self.state.external,
            "session_name": self.state.session, "duration_seconds": 900, "sts_region": self.state.region, "sts_endpoint": self.endpoint + "/sts",
            "source": {"access_key_id": self.state.base[0], "secret_access_key": self.state.base[1], "session_token": self.state.base[2]}}}

    def run_sts_graph(self, count, sequential=False, success=True):
        spec = {"nodes": [{"alias": f"user{i}", "address": "stackql_unstable_awsfixture.iam.users"} for i in range(count)],
                "args": {"auth_by_provider": {"stackql_unstable_awsfixture": self.auth}}}
        if sequential:
            spec["wirings"] = [{"to": "user1", "inbound": [{"from": "user0", "src": "id", "as": "previous"}]}]
        result = subprocess.run([self.binary, "doc-graph", str(self.root / "registry"), json.dumps(spec), "--endpoint", self.endpoint, "--aws-region", "ap-southeast-2"], capture_output=True, text=True, timeout=20)
        assert (result.returncode == 0) == success, result.stdout + result.stderr
        for secret in [self.state.base[1], self.state.base[2], "ROLESECRET", "ROLETOKEN", self.state.external]:
            assert secret not in result.stdout + result.stderr, "credential/error detail leaked"
        if success:
            assert "role-user" in result.stdout
        return result

    def concurrent_queries_share_assume_role(self):
        self.run_sts_graph(12)
        calls = self.state.requests
        assert sum(call["action"] == "AssumeRole" for call in calls) == 1, calls
        downstream = [call for call in calls if call["action"] == "ListUsers"]
        assert len(downstream) == 12 and {call["key"] for call in downstream} == {"ROLEKEY1"}, calls

    def expired_role_credentials_refresh_before_next_request(self):
        self.state.lifetime = 2
        self.state.downstream_delay = 2.2
        self.run_sts_graph(2, sequential=True)
        assert [call["action"] for call in self.state.requests] == ["AssumeRole", "ListUsers", "AssumeRole", "ListUsers"], self.state.requests
        assert [call["key"] for call in self.state.requests if call["action"] == "ListUsers"] == ["ROLEKEY1", "ROLEKEY2"]

    def rejected_assume_role_never_dispatches_downstream(self):
        self.state.failure = True
        self.run_sts_graph(1, success=False)
        assert self.state.requests and all(call["action"] == "AssumeRole" for call in self.state.requests), self.state.requests

    def expired_sts_response_never_dispatches_downstream(self):
        self.state.expired = True
        self.run_sts_graph(1, success=False)
        assert self.state.requests and all(call["action"] == "AssumeRole" for call in self.state.requests), self.state.requests

    def stop_sts(self):
        self.state.block.set()
        self.server.shutdown()
        self.worker.join(5)
        shutil.rmtree(self.root)
