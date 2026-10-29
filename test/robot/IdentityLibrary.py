"""CLI lifecycle checks against an isolated, credential-free API."""
import copy
import json
from pathlib import Path
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse


class IdentityLibrary:
    ROBOT_LIBRARY_SCOPE = "TEST"

    def start_identity_lifecycle(self, binary, mode):
        self.live_profile = None
        self.cli_env = {}
        self.binary = str(Path(binary).resolve())
        self.mode = mode
        self.root = Path(tempfile.mkdtemp(prefix="omni-identity-"))
        self.items, self.operations, self.events = {}, {}, []
        self.lock = threading.Lock()
        self.serial = 0
        owner = self

        class API(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def dispatch(self):
                if owner.live_profile:
                    owner.handle_google(self)
                    return
                with owner.lock:
                    path = urlparse(self.path)
                    body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
                    code, answer = 200, {}
                    if path.path.startswith("/operations/"):
                        op = owner.operations[path.path.rsplit("/", 1)[1]]
                        op["polls"] += 1
                        done = op["polls"] >= 2
                        answer = {"operation": op["operation"], "status": "DONE" if done else "RUNNING"}
                        if done:
                            owner.items[op["id"]] = op["body"]
                            owner.events.append(("ready", op["id"]))
                            if owner.mode == "await-late":
                                answer["id"] = op["id"]
                            if owner.mode == "await-error":
                                answer["error"] = {"message": "operation rejected"}
                        if owner.mode == "await-timeout":
                            answer["status"] = "RUNNING"
                    elif self.command == "GET" and path.path == "/widgets":
                        identity = parse_qs(path.query).get("id", [""])[0]
                        answer = [copy.deepcopy(owner.items[identity])] if identity in owner.items else []
                    elif self.command == "POST" and path.path == "/widgets":
                        parent = body.get("parent")
                        if parent and parent not in owner.items:
                            code, answer = 409, {"error": "parent not ready"}
                        else:
                            owner.serial += 1
                            identity = parse_qs(path.query)["name"][0] if owner.mode == "caller-name" else f"generated-{owner.serial}"
                            if identity in owner.items:
                                code, answer = 409, {"error": "duplicate create"}
                            else:
                                body["id"] = identity
                                owner.events.append(("create", identity))
                                if owner.mode.startswith("await-"):
                                    operation = f"op-{owner.serial}"
                                    owner.operations[operation] = {"operation": operation, "id": identity, "body": body, "polls": 0}
                                    answer = {"operation": operation, "status": "RUNNING"}
                                    if owner.mode == "await-eager":
                                        answer["id"] = identity
                                else:
                                    owner.items[identity] = body
                                    answer = {} if owner.mode == "caller-name" else {"id": identity}
                    elif path.path.startswith("/widgets/"):
                        identity = path.path.rsplit("/", 1)[1]
                        if identity not in owner.items:
                            code, answer = 404, {"error": "missing"}
                        elif self.command == "PATCH":
                            owner.items[identity].update(body)
                            owner.events.append(("update", identity))
                            answer = copy.deepcopy(owner.items[identity])
                        elif self.command == "DELETE":
                            if any(item.get("parent") == identity for item in owner.items.values()):
                                code, answer = 409, {"error": "retained child"}
                            else:
                                del owner.items[identity]
                                owner.events.append(("delete", identity))
                                code = 204
                    else:
                        code, answer = 404, {"error": "unexpected request"}
                    payload = b"" if code == 204 else json.dumps(answer).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            do_GET = do_POST = do_PATCH = do_DELETE = dispatch

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), API)
        self.worker = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.worker.start()
        self.endpoint = f"http://127.0.0.1:{self.server.server_port}"
        self.write_registry()
        self.resources = []
        for key in ["parent", "child"]:
            resource = {"key": key, "provider": "widgets", "address": "things.widgets", "identity": "id", "addressed_by": "id",
                        "desired": {"color": "first"}}
            if mode == "caller-name":
                resource["identity"] = ""
                resource["write_once"] = {"id": key, "name": key}
            if key == "child":
                resource["inbound"] = [{"from": "parent", "as": "parent"}]
            self.resources.append(resource)
        self.run = 0

    def write_registry(self):
        source = Path(__file__).resolve().parents[2] / "cicd/examples/identity-lifecycle/mock-registry"
        provider = json.loads((source / "provider.json").read_text())
        doc = json.loads((source / "things.json").read_text())
        props = doc["components"]["schemas"]["Widget"]["properties"]
        props.update({"name": {"type": "string"}, "parent": {"type": "string"}})
        doc["paths"]["/widgets"]["post"]["parameters"] = [
            {"name": name, "in": "query", "required": False, "schema": {"type": "string"}}
            for name in ["id", "name"]]
        if self.mode.startswith("await-"):
            doc["components"]["schemas"]["Operation"] = {"type": "object", "properties": {
                "operation": {"type": "string"}, "status": {"type": "string"}, "id": {"type": "string"}, "error": {"type": "object"}}}
            doc["paths"]["/widgets"]["post"]["responses"]["200"]["content"]["application/json"]["schema"] = {"$ref": "#/components/schemas/Operation"}
            doc["paths"]["/operations/{operation}"] = {"get": {"operationId": "poll_operation", "parameters": [
                {"name": "operation", "in": "path", "required": True, "schema": {"type": "string"}}],
                "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Operation"}}}}}}}
            methods = doc["components"]["x-stackQL-resources"]["widgets"]["methods"]
            methods["poll"] = {"operation": {"$ref": "#/paths/~1operations~1{operation}/get"}, "response": {"mediaType": "application/json", "openAPIDocKey": "200"}}
            methods["create"]["x-stackQL-beta"] = {
                "method": {"$ref": "#/components/x-stackQL-resources/widgets/methods/poll"}, "bind": {"operation": "operation"},
                "await": {"statusPath": "status", "done": "DONE", "errorPath": "error", "interval": "5ms", "maxAttempts": 3}}
        root = self.root / "registry/widgets/v00.00.00000"
        (root / "services").mkdir(parents=True)
        (root / "provider.yaml").write_text(json.dumps(provider))
        (root / "services/things.yaml").write_text(json.dumps(doc))

    def cli(self, arguments, expect_failure=False):
        self.run += 1
        if self.live_profile and arguments[0] == "iac-destroy":
            arguments = [*arguments, "--params", '{"project":"fixture"}']
        result = subprocess.run([self.binary, *arguments, "--name", "identity", "--state", str(self.root / "state"),
                                 "--run-id", f"run-{self.run}", "--endpoint", self.endpoint], capture_output=True, text=True, timeout=40, env={**__import__("os").environ, **self.cli_env})
        rows = [json.loads(line) for line in result.stdout.splitlines() if line.strip()]
        failed = result.returncode != 0 or any(row.get("error") or row.get("status") == "outstanding" for row in rows)
        if expect_failure != failed:
            raise AssertionError(f"rc={result.returncode} stdout={result.stdout} stderr={result.stderr}")
        return rows

    def apply_lifecycle(self, expect_failure=False):
        spec = self.root / "resources.json"
        spec.write_text(json.dumps({"resources": self.resources}))
        return self.cli(["iac-apply", str(self.root / "registry"), "@" + str(spec)], expect_failure)

    def create_and_check_identity(self):
        rows = self.apply_lifecycle()
        assert len(rows) == 2, rows
        self.identities = {row["key"].split("/")[-1]: row["identity"] for row in rows}
        assert all(self.identities.values()), rows
        if self.mode == "caller-name":
            assert self.identities == {"parent": "parent", "child": "child"}, rows
        else:
            assert len(set(self.identities.values())) == 2, rows
        assert len(self.items) == 2
        if self.mode.startswith("await-"):
            ready = self.events.index(("ready", self.identities["parent"]))
            child = self.events.index(("create", self.identities["child"]))
            assert ready < child, self.events
        self.check_no_op()

    def check_no_op(self):
        before = list(self.events)
        self.apply_lifecycle()
        assert self.events == before, (before, self.events)

    def update_repeatedly(self, count=3):
        for index in range(int(count)):
            for resource in self.resources:
                resource["desired"]["color"] = f"revision-{index}"
            before = sum(event[0] == "update" for event in self.events)
            rows = self.apply_lifecycle()
            assert sum(event[0] == "update" for event in self.events) == before + len(self.resources), self.events
            for row in rows:
                assert row["identity"] == self.identities[row["key"].split("/")[-1]], rows
            assert all(item["color"] == f"revision-{index}" for item in self.items.values()), self.items
            self.check_no_op()

    def prune_child_and_continue(self):
        before = list(self.events)
        self.cli(["iac-destroy", "--registry", str(self.root / "registry"), "--key", "parent"], True)
        assert self.events == before, "retained dependency rejection sent a delete"
        self.cli(["iac-destroy", "--registry", str(self.root / "registry"), "--key", "child"])
        self.resources = self.resources[:1]
        assert set(self.items) == {self.identities["parent"]}, self.items
        self.update_repeatedly(2)

    def generate_local_key_and_use_public_digest(self):
        key = self.root / "local-private.pem"
        subprocess.run(["openssl", "genpkey", "-algorithm", "ED25519", "-out", str(key)], check=True, capture_output=True, timeout=10)
        key.chmod(0o600)
        public = subprocess.run(["openssl", "pkey", "-in", str(key), "-pubout", "-outform", "DER"], check=True, capture_output=True, timeout=10).stdout
        import hashlib
        digest = hashlib.sha256(public).hexdigest()
        for resource in self.resources:
            resource["desired"]["color"] = {"from": "input", "attribute": "public_digest"}
        spec = self.root / "resources.json"
        spec.write_text(json.dumps({"resources": self.resources}))
        self.cli(["iac-apply", str(self.root / "registry"), "@" + str(spec), "--input", json.dumps({"public_digest": digest})])
        assert all(item["color"] == digest for item in self.items.values()), self.items

    def destroy_and_check_empty(self):
        self.cli(["iac-destroy", "--registry", str(self.root / "registry")])
        assert not self.items, self.items
        before = list(self.events)
        self.cli(["iac-destroy", "--registry", str(self.root / "registry")])
        assert before == self.events

    def await_failure_blocks_dependents(self):
        self.apply_lifecycle(True)
        assert len([event for event in self.events if event[0] == "create"]) == 1, self.events
        before = list(self.events)
        self.cli(["iac-destroy", "--registry", str(self.root / "registry")], True)
        assert self.events == before, "unconfirmed identity was deleted"

    def stop_identity_lifecycle(self):
        if hasattr(self, "server"):
            self.server.shutdown()
            self.server.server_close()
            self.worker.join(timeout=5)
        if hasattr(self, "root"):
            import shutil
            shutil.rmtree(self.root)

    def handle_google(self, request):
        with self.lock:
            assert request.headers.get("Authorization") == "Bearer fixture-token"
            path = urlparse(request.path).path.removeprefix("/compute/v1").removeprefix("/storage/v1")
            body = json.loads(request.rfile.read(int(request.headers.get("Content-Length", 0))) or b"{}")
            bucket = self.live_profile == "caller-name"
            collection = "/b" if bucket else "/projects/fixture/global/networks"
            code, result = 200, {}
            if "/operations/" in path:
                op = self.operations[path.rsplit("/", 1)[1]]
                op["polls"] += 1
                result = {"name": op["name"], "status": "RUNNING"}
                if op["polls"] >= 2:
                    result.update(status="DONE", targetLink=op["targetLink"])
                    if op["verb"] == "DELETE":
                        self.items.pop(op["id"], None)
                    else:
                        self.items[op["id"]] = op["body"]
            elif request.command == "POST" and path == collection:
                identity = body["name"]
                assert identity not in self.items, "duplicate create"
                self.events.append(("create", identity))
                if bucket:
                    self.items[identity] = body
                    result = body
                else:
                    result = self.google_operation(identity, body, "POST")
            elif path.startswith(collection + "/"):
                identity = path.rsplit("/", 1)[1]
                if request.command == "GET":
                    if identity not in self.items:
                        code, result = 404, {"error": {"code": 404}}
                    else:
                        result = copy.deepcopy(self.items[identity])
                elif request.command == "PATCH":
                    updated = copy.deepcopy(self.items[identity])
                    updated.update(body)
                    self.events.append(("update", identity))
                    if bucket:
                        self.items[identity] = updated
                        result = updated
                    else:
                        result = self.google_operation(identity, updated, "PATCH")
                elif request.command == "DELETE":
                    self.events.append(("delete", identity))
                    if bucket:
                        del self.items[identity]
                        code = 204
                    else:
                        result = self.google_operation(identity, {}, "DELETE")
            else:
                code, result = 404, {"error": "unexpected request"}
            payload = b"" if code == 204 else json.dumps(result).encode()
        request.send_response(code)
        request.send_header("Content-Type", "application/json")
        request.send_header("Content-Length", str(len(payload)))
        request.end_headers()
        request.wfile.write(payload)

    def google_operation(self, identity, body, verb):
        name = f"operation-{len(self.operations) + 1}"
        target = "https://www.googleapis.com/compute/v1/projects/fixture/global/networks/" + identity
        self.operations[name] = {"name": name, "targetLink": target, "body": body, "polls": 0, "id": identity, "verb": verb}
        # Real Compute Engine can return the target before completion in either profile.
        return {"name": name, "targetLink": target, "status": "RUNNING"}

    def run_live_google_document_lifecycle(self, binary, profile):
        self.start_identity_lifecycle(binary, "eager")
        self.live_profile = profile
        self.cli_env = {"OMNI_LIFECYCLE_TOKEN": "fixture-token"}
        import runpy
        from types import SimpleNamespace
        source = Path(__file__).resolve().parents[2] / "cicd/examples/identity-lifecycle/prepare.py"
        prepare = runpy.run_path(str(source))["prepare"]
        root = self.root / "live"
        prepare(SimpleNamespace(out=root, profile=profile, project="fixture", location="AUSTRALIA-SOUTHEAST1", prefix="fixture"))
        self.registry = root / "registry"
        document = self.registry / "lifecycle/v1/services/cloud.yaml"
        doc = json.loads(document.read_text())
        for method in doc["components"]["x-stackQL-resources"]["objects"]["methods"].values():
            if "x-stackQL-beta" in method:
                method["x-stackQL-beta"]["await"]["interval"] = "5ms"
        document.write_text(json.dumps(doc))
        def apply(stage):
            return self.cli(["iac-apply", str(self.registry), "@" + str(root / (stage + ".json"))])
        first = apply("01-create")
        identities = {row["key"]: row["identity"] for row in first}
        assert set(identities.values()) == {"fixture-main", "fixture-secondary"}, first
        for stage in ["01-create", "02-update", "03-update", "04-update"]:
            rows = apply(stage)
            assert {row["key"]: row["identity"] for row in rows} == identities, rows
            expected = json.loads((root / (stage + ".json")).read_text())
            for resource in expected["resources"]:
                for name, value in resource["desired"].items():
                    assert self.items[resource["desired"]["name"]][name] == value, (stage, self.items)
            before = list(self.events)
            apply(stage)
            assert self.events == before, (stage, self.events)
        self.cli(["iac-destroy", "--registry", str(self.registry), "--key", "secondary"])
        assert set(self.items) == {"fixture-main"}, self.items
        apply("05-after-prune")
        before = list(self.events)
        apply("05-after-prune")
        assert self.events == before, self.events
        # Generate a real local key for every live-document profile too.
        key = self.root / "private.pem"
        subprocess.run(["openssl", "genpkey", "-algorithm", "ED25519", "-out", str(key)], check=True, capture_output=True, timeout=10)
        key.chmod(0o600)
        public = subprocess.run(["openssl", "pkey", "-in", str(key), "-pubout", "-outform", "DER"], check=True, capture_output=True, timeout=10).stdout
        import hashlib
        digest = hashlib.sha256(public).hexdigest()[:32]
        command = ["iac-apply", str(self.registry), "@" + str(root / "06-local.json"), "--input", json.dumps({"public_digest": digest})]
        self.cli(command)
        if profile == "caller-name":
            assert self.items["fixture-main"]["labels"]["public_key"] == digest
        before = list(self.events)
        self.cli(command)
        assert self.events == before
        self.cli(["iac-destroy", "--registry", str(self.registry)])
        assert not self.items, self.items

    def selected_deletion_is_atomic_and_ordered(self, binary):
        self.start_identity_lifecycle(binary, "eager")
        self.create_and_check_identity()
        before = list(self.events)
        for keys in [["missing"], ["child", "missing"], [""]]:
            args = ["iac-destroy", "--registry", str(self.root / "registry")]
            for key in keys:
                args += ["--key", key]
            self.cli(args, True)
            assert self.events == before, "invalid selection caused partial deletion"
        self.cli(["iac-destroy", "--registry", str(self.root / "registry"), "--key", "parent", "--key", "child"])
        assert self.events[-2:] == [("delete", self.identities["child"]), ("delete", self.identities["parent"])], self.events
        assert not self.items

    def existing_caller_named_object_is_not_adopted(self, binary):
        self.start_identity_lifecycle(binary, "caller-name")
        external = {"id": "parent", "color": "external-owner"}
        self.items["parent"] = copy.deepcopy(external)
        self.apply_lifecycle(True)
        assert self.items == {"parent": external}, self.items
        assert not any(event[0] == "delete" for event in self.events), self.events
