"""JWT caller policy through real CLI processes; provider calls are observed externally."""
import base64
import copy
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ed25519, padding, rsa


def encoded(value):
    return base64.urlsafe_b64encode(value).rstrip(b"=").decode()


class JWTAccessLibrary:
    ROBOT_LIBRARY_SCOPE = "SUITE"

    def prepare_jwt_access(self, binary):
        self.binary = str(Path(binary).resolve())
        self.root = Path(tempfile.mkdtemp(prefix="omni-jwt-access-"))
        self.calls, self.lock = [], threading.Lock()
        owner = self

        class API(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def dispatch(self):
                with owner.lock:
                    owner.calls.append((self.command, self.path))
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                body = json.dumps([{"id": "one", "name": "readable"}] if self.command == "GET" else {"id": "created"}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            do_GET = do_POST = do_PATCH = do_DELETE = dispatch

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), API)
        self.worker = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.worker.start()
        self.endpoint = f"http://127.0.0.1:{self.server.server_port}"
        self.keys = {"EdDSA": ed25519.Ed25519PrivateKey.generate(), "RS256": rsa.generate_private_key(public_exponent=65537, key_size=2048)}
        self.config = {
            "issuer": "https://issuer.example.test", "mechanism": "jwt",
            "jwt": {"audience": "stackql-test", "algorithms": ["EdDSA", "RS256"], "keys": [
                {"kid": algorithm, "algorithm": algorithm, "public_key_pem": key.public_key().public_bytes(serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo).decode()}
                for algorithm, key in self.keys.items()]},
            "directory": {"groups": ["readers", "operators"], "principals": [
                {"id": "reader", "identities": [{"issuer": "https://issuer.example.test", "subject": "reader-subject"}], "groups": ["readers"]},
                {"id": "operator", "identities": [{"issuer": "https://issuer.example.test", "subject": "operator-subject"}], "groups": ["operators"]}]},
            "rules": [
                {"subject": "group:readers", "resource": "command/omnicli/*", "action": "execute"},
                {"subject": "group:readers", "resource": "credential/demo", "action": "use"},
                {"subject": "group:readers", "resource": "operation/demo/list/listItems", "action": "execute"},
                {"subject": "group:operators", "resource": "command/omnicli/*", "action": "execute"},
                {"subject": "group:operators", "resource": "credential/demo", "action": "use"},
                {"subject": "group:operators", "resource": "operation/demo/**", "action": "execute"}]}
        self.write_registry()

    def write_registry(self):
        schema = {"type": "object", "properties": {"id": {"type": "string"}, "name": {"type": "string"}}}
        response = {"200": {"content": {"application/json": {"schema": schema}}}}
        param = {"name": "id", "in": "path", "required": True, "schema": {"type": "string"}}
        body = {"content": {"application/json": {"schema": schema}}}
        paths = {"/items": {
            "get": {"operationId": "listItems", "responses": {"200": {"content": {"application/json": {"schema": {"type": "array", "items": schema}}}}}},
            "post": {"operationId": "createItem", "requestBody": body, "responses": response}},
            "/items/{id}": {"patch": {"operationId": "updateItem", "parameters": [param], "requestBody": body, "responses": response},
                            "delete": {"operationId": "deleteItem", "parameters": [param], "responses": response}},
            "/action": {"post": {"operationId": "runAction", "responses": response}}}
        methods, verbs = {}, {}
        for verb, name, path, http in [("select", "list", "/items", "get"), ("insert", "create", "/items", "post"),
                                       ("update", "update", "/items/{id}", "patch"), ("delete", "delete", "/items/{id}", "delete"),
                                       ("exec", "action", "/action", "post")]:
            methods[name] = {"operation": {"$ref": "#/paths/" + path.replace("/", "~1") + "/" + http},
                             "response": {"mediaType": "application/json", "openAPIDocKey": "200"}}
            verbs[verb] = [{"$ref": "#/components/x-stackQL-resources/items/methods/" + name}]
        doc = {"openapi": "3.0.0", "servers": [{"url": "https://example.test"}], "paths": paths,
               "components": {"x-stackQL-resources": {"items": {"methods": methods, "sqlVerbs": verbs}}}}
        (self.root / "provider.yaml").write_text(json.dumps({"name": "demo", "providerServices": {"service": {"service": {"$ref": "service.yaml"}}}, "config": {"auth": {"type": "null_auth"}}}))
        (self.root / "service.yaml").write_text(json.dumps(doc))

    def token(self, algorithm="EdDSA", variant="valid", subject="reader-subject"):
        now = int(time.time())
        header = {"alg": algorithm, "typ": "JWT", "kid": algorithm}
        # These token-owned groups MUST NOT grant operator rights.
        claims = {"iss": "https://issuer.example.test", "aud": "stackql-test", "sub": subject,
                  "iat": now - 1, "nbf": now - 1, "exp": now + 300, "groups": ["operators"]}
        if variant == "wrong-issuer":
            claims["iss"] = "https://other.example.test"
        elif variant == "wrong-audience":
            claims["aud"] = "another-service"
        elif variant == "expired":
            claims["exp"] = now - 30
        elif variant == "future-nbf":
            claims["nbf"] = now + 300
        elif variant == "future-iat":
            claims["iat"] = now + 300
        elif variant == "no-expiry":
            del claims["exp"]
        elif variant == "no-subject":
            del claims["sub"]
        elif variant == "unknown-subject":
            claims["sub"] = "not-in-directory"
        elif variant == "unknown-key":
            header["kid"] = "untrusted"
        elif variant == "none":
            header["alg"] = "none"
        elif variant == "hmac-confusion":
            header["alg"] = "HS256"
        message = (encoded(json.dumps(header).encode()) + "." + encoded(json.dumps(claims).encode())).encode()
        key = self.keys[algorithm]
        if variant == "tampered":
            key = ed25519.Ed25519PrivateKey.generate() if algorithm == "EdDSA" else rsa.generate_private_key(public_exponent=65537, key_size=2048)
        signature = key.sign(message) if algorithm == "EdDSA" else key.sign(message, padding.PKCS1v15(), hashes.SHA256())
        if variant == "none":
            signature = b""
        elif variant == "hmac-confusion":
            import hmac
            signature = hmac.digest(self.config["jwt"]["keys"][0]["public_key_pem"].encode(), message, "sha256")
        return message.decode() + "." + encoded(signature)

    def invoke(self, command, token, config=None, expect_success=True, expected_calls=0):
        cfg_path = self.root / "access.json"
        cfg_path.write_text(json.dumps(self.config if config is None else config))
        cfg_path.chmod(0o600)
        with self.lock:
            before = len(self.calls)
        result = subprocess.run([self.binary, *command, "--access-config", str(cfg_path), "--caller-token-env", "OMNI_JWT_TEST_TOKEN", "--endpoint", self.endpoint],
                                env={**os.environ, "OMNI_JWT_TEST_TOKEN": token}, capture_output=True, text=True, timeout=30)
        if expect_success:
            assert result.returncode == 0, result.stderr + result.stdout
        else:
            assert result.returncode != 0, result.stdout
        with self.lock:
            assert len(self.calls) - before == expected_calls, self.calls[before:]
        assert not token or token not in result.stdout + result.stderr, "caller credential leaked"
        return result

    def reader_select_succeeds(self, algorithm="EdDSA"):
        result = self.invoke(["doc-run", str(self.root), "stackql_unstable_demo.service.items"], self.token(algorithm), expected_calls=1)
        assert json.loads(result.stdout)["name"] == "readable", result.stdout

    def reader_mutation_is_denied(self, verb):
        spec = {"nodes": [{"alias": "mutation", "address": "stackql_unstable_demo.service.items", "verb": verb,
                           "params": {"id": "one"} if verb in ["update", "delete"] else {}, "body": {"name": "forbidden"}}]}
        self.invoke(["doc-graph", str(self.root), json.dumps(spec)], self.token(), expect_success=False)

    def reader_iac_is_denied(self):
        spec = self.root / "resources.json"
        spec.write_text(json.dumps({"resources": [{"key": "one", "provider": "demo", "address": "service.items", "identity": "id", "addressed_by": "id", "desired": {"name": "forbidden"}}]}))
        state = self.root / "denied-state"
        self.invoke(["iac-apply", str(self.root), "@" + str(spec), "--name", "protected", "--state", str(state), "--run-id", "denied"], self.token(), expect_success=False)
        assert not state.exists(), "denied collection access touched durable state"
        self.invoke(["iac-destroy", "--registry", str(self.root), "--name", "protected", "--state", str(state)], self.token(), expect_success=False)
        assert not state.exists()

    def jwt_variant_is_denied(self, variant):
        self.invoke(["doc-run", str(self.root), "stackql_unstable_demo.service.items"], "" if variant == "missing" else self.token(variant=variant), expect_success=False)

    def jwt_groups_come_from_directory(self):
        result = self.invoke(["groups"], self.token())
        assert json.loads(result.stdout) == {"groups": ["readers"]}, result.stdout
        result = self.invoke(["whoami"], self.token())
        assert json.loads(result.stdout) == {"principal": "reader"}, result.stdout

    def operator_mutation_succeeds(self):
        spec = {"nodes": [{"alias": "create", "address": "stackql_unstable_demo.service.items", "verb": "insert", "body": {"name": "allowed"}}]}
        self.invoke(["doc-graph", str(self.root), json.dumps(spec)], self.token(subject="operator-subject"), expected_calls=1)

    def directory_and_policy_fail_closed(self):
        for variation in ["unknown", "credential", "operation", "membership"]:
            cfg = copy.deepcopy(self.config)
            if variation == "unknown":
                cfg["directory"]["principals"] = cfg["directory"]["principals"][1:]
            elif variation == "membership":
                cfg["directory"]["principals"][0]["groups"] = []
            else:
                cfg["rules"] = [r for r in cfg["rules"] if r["resource"] != ("credential/demo" if variation == "credential" else "operation/demo/list/listItems")]
            self.invoke(["doc-run", str(self.root), "stackql_unstable_demo.service.items"], self.token(), config=cfg, expect_success=False)

    def stop_jwt_access(self):
        if hasattr(self, "server"):
            self.server.shutdown()
            self.server.server_close()
            self.worker.join(timeout=5)
        if hasattr(self, "root"):
            shutil.rmtree(self.root)
