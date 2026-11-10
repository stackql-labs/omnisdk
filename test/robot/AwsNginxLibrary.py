"""Real merged AWS documents and SDK ledger, with a strict EC2 Query mock."""
import base64
import json
from pathlib import Path
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse
from xml.sax.saxutils import escape


class AwsNginxLibrary:
    ROBOT_LIBRARY_SCOPE = "SUITE"

    def prepare_aws_nginx(self, binary):
        self.binary = str(Path(binary).resolve())
        self.repo = Path(__file__).resolve().parents[2]
        self.root = Path(tempfile.mkdtemp(prefix="omni-aws-nginx-"))
        self.events, self.lock = [], threading.Lock()
        self.checkpoints = []
        self.lose_create_response = False
        self.lose_delete_response = False
        self.pause_poll = False
        self.poll_started = threading.Event()
        self.allow_poll = threading.Event()
        self.subnets, self.associations = set(), set()
        self.vpc = self.gateway = self.attached = self.routes = self.route = self.group = self.ingress = self.key = False
        self.instance = None
        self.polls = 0
        self.network_polls = {}
        owner = self

        class API(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_POST(self):
                form = parse_qs(self.rfile.read(int(self.headers.get("Content-Length", "0"))).decode())
                form.update(parse_qs(urlparse(self.path).query))
                values = {key: value[0] for key, value in form.items()}
                action = values.get("Action", "")
                if action == "DescribeInstances" and owner.pause_poll:
                    owner.poll_started.set()
                    owner.allow_poll.wait(30)
                    self.close_connection = True
                    return
                try:
                    with owner.lock:
                        owner.events.append({"action": action, "params": values})
                        owner.check_checkpoint(action)
                        fields = owner.dispatch(action, values)
                    if (action == "RunInstances" and owner.lose_create_response) or (action == "TerminateInstances" and owner.lose_delete_response):
                        self.close_connection = True
                        return  # Remote mutation succeeded; the caller receives no response.
                    body = f'<{action}Response xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">{fields}</{action}Response>'.encode()
                    self.send_response(200)
                except (AssertionError, KeyError) as error:
                    body = f'<Response><Errors><Error><Code>DependencyViolation</Code><Message>{escape(str(error))}</Message></Error></Errors></Response>'.encode()
                    self.send_response(400)
                self.send_header("Content-Type", "application/xml")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), API)
        self.worker = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.worker.start()
        self.endpoint = f"http://127.0.0.1:{self.server.server_port}"
        self.example = self.repo / "cicd/examples/aws-nginx"
        subprocess.run(["python3", str(self.example / "prepare.py"), "--case", str(self.root),
                        "--zone-a", "ap-southeast-2a", "--zone-b", "ap-southeast-2b",
                        "--ssh-cidr", "203.0.113.7/32", "--prefix", "robot-nginx", "--poll-interval", "5ms", "--poll-attempts", "10"], check=True)
        result = subprocess.run([self.binary, "doc-merge", str(self.repo / "test/corpus/registry"), "@"+str(self.root / "merge.json")], capture_output=True, text=True)
        assert result.returncode == 0, result.stderr
        self.registry = result.stdout.strip()
        assert not self.events, "preparation contacted AWS"

    def command(self, command):
        scope = ["--name", "robot-nginx", "--state", str(self.root / "state"), "--aws-region", "ap-southeast-2", "--endpoint", self.endpoint]
        if command in {"apply", "reapply", "stop"}:
            return [self.binary, "iac-apply", self.registry, "@"+str(self.example / ("stopped.json" if command == "stop" else "resources.json")),
                    "--input", "@"+str(self.root / "inputs.json"), "--run-id", "create" if command == "apply" else command+"-"+str(__import__("time").time_ns()), *scope]
        if command == "destroy":
            return [self.binary, "iac-destroy", "--registry", self.registry, "--run-id", "destroy-"+str(__import__("time").time_ns()), *scope]
        if command == "status":
            return [self.binary, "doc-run", self.registry, "stackql_unstable_aws.ec2.web_instance", json.dumps({"params":{"InstanceId.1":"i-web"}}), "--aws-region", "ap-southeast-2", "--endpoint", self.endpoint]
        raise AssertionError(command)

    def cli(self, command, *arguments, expect_success=True):
        result = subprocess.run(self.command(command), capture_output=True, text=True, timeout=60,
                                env={**__import__("os").environ, "AWS_ACCESS_KEY_ID": "MOCKACCESS", "AWS_SECRET_ACCESS_KEY": "mock-secret"})
        (self.root / f"{command}.stdout.txt").write_text(result.stdout)
        (self.root / f"{command}.stderr.txt").write_text(result.stderr)
        with self.lock:
            (self.root / "api-transcript.json").write_text(json.dumps(self.events, indent=2))
        assert (result.returncode == 0) == expect_success, f"{command}: {result.stdout}\n{result.stderr}\ncase: {self.root}"
        return result

    def ledger_snapshot(self):
        collection = "robot-nginx"
        directory = self.root / "state" / "ledger" / collection
        latest = {}
        for file in directory.glob("*.v*.json"):
            stem, version = file.name.rsplit(".v", 1)
            number = int(version.removesuffix(".json"))
            if stem not in latest or number > latest[stem][0]:
                latest[stem] = (number, json.loads(file.read_text()))
        return {key: data for key, (_, data) in latest.items() if not data.get("tombstone")}

    def check_checkpoint(self, action):
        if action not in {"CreateVpc", "ImportKeyPair", "RunInstances", "DescribeInstances", "StopInstances", "TerminateInstances", "DeleteVpc"}:
            return
        snapshot = self.ledger_snapshot()
        claims = json.loads(base64.b64decode(snapshot[".lease"]["proposed"]))["claims"] or []
        owned = set()
        for claim in claims:
            keys = set(claim.get("keys", []))
            assert not owned.intersection(keys), "overlapping write leases"
            owned.update(keys)
        target = {"CreateVpc": "vpc", "ImportKeyPair": "key", "RunInstances": "instance", "StopInstances": "instance", "TerminateInstances": "instance", "DeleteVpc": "vpc"}.get(action)
        if target:
            assert snapshot[target]["phase"] == 0, (action, snapshot[target])
            assert any(c.get("all") or any(k.endswith("/" + target) for k in c.get("keys", [])) for c in claims), (action, "missing write lock")
        if action == "RunInstances":
            # Reconciliation currently retains its bounded footprint through the run.
            assert len([k for k in snapshot if not k.startswith(".")]) == 13
            assert all(v["phase"] == 1 for k, v in snapshot.items() if k not in {".lease", "instance"})
        if action == "DescribeInstances" and self.instance == "pending":
            assert snapshot["instance"]["phase"] == 0, "readiness published early"
            journal = [json.loads(line) for line in (self.root / "state/journal/create.jsonl").read_text().splitlines()]
            assert any(r.get("await_inputs", {}).get("InstanceId.1") == "i-web" for r in journal), "poll handle not durable"
        if action == "StopInstances":
            about = json.loads(base64.b64decode(snapshot["instance"]["about"]))
            assert about.get("depends_on"), "stop lost dependency metadata"
        if action == "DeleteVpc":
            assert [k for k in snapshot if not k.startswith(".")] == ["vpc"], "VPC deleted before dependents"
        self.checkpoints.append({"action": action, "ledger": snapshot, "claims": claims})
        (self.root / "checkpoints.json").write_text(json.dumps(self.checkpoints, indent=2))

    def assert_ledger_settled(self, expected):
        snapshot = self.ledger_snapshot()
        resources = {k: v for k, v in snapshot.items() if not k.startswith(".")}
        assert len(resources) == expected, resources
        assert all(v["phase"] == 1 and v.get("identity") for v in resources.values()), resources
        claims = json.loads(base64.b64decode(snapshot[".lease"]["proposed"]))["claims"]
        assert not claims, "write locks leaked after phase completion"

    def dispatch(self, action, p):
        if action == "DescribeInternetGateways":
            attachment = '<attachmentSet><item><vpcId>vpc-web</vpcId><state>available</state></item></attachmentSet>' if self.attached else ''
            return f'<internetGatewaySet><item><internetGatewayId>igw-web</internetGatewayId>{attachment}</item></internetGatewaySet>'
        if action == "DescribeRouteTables":
            assert p["RouteTableId.1"] == "rtb-web"
            associations = ''.join(f'<item><routeTableAssociationId>{a}</routeTableAssociationId></item>' for a in sorted(self.associations))
            routes = '<routeSet><item><destinationCidrBlock>0.0.0.0/0</destinationCidrBlock><gatewayId>igw-web</gatewayId></item></routeSet>' if self.route else ''
            return f'<routeTableSet><item><routeTableId>rtb-web</routeTableId><associationSet>{associations}</associationSet>{routes}</item></routeTableSet>'
        if action == "DescribeSecurityGroups":
            rules = '<ipPermissions><item><fromPort>22</fromPort><toPort>22</toPort><ipProtocol>tcp</ipProtocol></item><item><fromPort>80</fromPort><toPort>80</toPort><ipProtocol>tcp</ipProtocol></item></ipPermissions>' if self.ingress else ''
            return f'<securityGroupInfo><item><groupId>sg-web</groupId>{rules}</item></securityGroupInfo>'
        if action == "DescribeKeyPairs":
            return '<keySet><item><keyName>robot-nginx-ssh</keyName></item></keySet>'
        if action == "CreateVpc":
            self.vpc = True
            return '<vpc><vpcId>vpc-web</vpcId><state>available</state></vpc>'
        if action in {"DescribeVpcs", "DescribeSubnets"}:
            vpc = action == "DescribeVpcs"
            identity = p["VpcId.1" if vpc else "SubnetId.1"]
            self.network_polls[identity] = self.network_polls.get(identity, 0) + 1
            state = "available" if self.network_polls[identity] >= 2 else "pending"
            container, field = ("vpcSet", "vpcId") if vpc else ("subnetSet", "subnetId")
            return f"<{container}><item><{field}>{identity}</{field}><state>{state}</state></item></{container}>"
        if action == "CreateSubnet":
            assert self.vpc and p["VpcId"] == "vpc-web" and self.network_polls.get("vpc-web", 0) >= 2
            identity = "subnet-a" if p["CidrBlock"] == "10.77.1.0/24" else "subnet-b"
            self.subnets.add(identity)
            return f'<subnet><subnetId>{identity}</subnetId><vpcId>vpc-web</vpcId><state>available</state></subnet>'
        if action == "CreateInternetGateway":
            self.gateway = True
            return '<internetGateway><internetGatewayId>igw-web</internetGatewayId></internetGateway>'
        if action == "AttachInternetGateway":
            assert self.vpc and self.gateway
            assert p["InternetGatewayId"] == "igw-web" and p["VpcId"] == "vpc-web"
            self.attached = True
        elif action == "CreateRouteTable":
            assert self.vpc and p["VpcId"] == "vpc-web"
            self.routes = True
            return '<routeTable><routeTableId>rtb-web</routeTableId><vpcId>vpc-web</vpcId></routeTable>'
        elif action == "CreateRoute":
            assert self.routes and self.attached
            assert p["GatewayId"] == "igw-web" and p["DestinationCidrBlock"] == "0.0.0.0/0"
            self.route = True
        elif action == "AssociateRouteTable":
            assert p["SubnetId"] in self.subnets and self.routes
            identity = "rtbassoc-" + p["SubnetId"]
            self.associations.add(identity)
            return f'<associationId>{identity}</associationId>'
        elif action == "CreateSecurityGroup":
            assert self.vpc and p["VpcId"] == "vpc-web"
            self.group = True
            return '<groupId>sg-web</groupId>'
        elif action == "AuthorizeSecurityGroupIngress":
            assert self.group and p["GroupId"] == "sg-web"
            assert p["IpPermissions.1.FromPort"] == "22" and p["IpPermissions.1.IpRanges.1.CidrIp"] == "203.0.113.7/32"
            assert p["IpPermissions.2.FromPort"] == "80"
            self.ingress = True
        elif action == "ImportKeyPair":
            assert base64.b64decode(p["PublicKeyMaterial"]).startswith(b"ssh-ed25519 ")
            self.key = True
            return f'<keyName>{p["KeyName"]}</keyName><keyPairId>key-web</keyPairId>'
        elif action == "RunInstances":
            assert p["ImageId"] == "resolve:ssm:/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64", "AMI default did not reach EC2"
            assert self.route and self.attached and self.key and self.ingress and self.associations
            assert self.network_polls.get("subnet-a", 0) >= 2
            assert p["NetworkInterface.1.SubnetId"] == "subnet-a"
            assert p["NetworkInterface.1.SecurityGroupId.1"] == "sg-web"
            assert p["NetworkInterface.1.AssociatePublicIpAddress"] == "true"
            assert p["KeyName"] == "robot-nginx-ssh" and p["MinCount"] == p["MaxCount"] == "1"
            script = base64.b64decode(p["UserData"]).decode()
            assert "dnf install -y nginx" in script and "robot-nginx" in script and "/healthz" in script
            assert p["MetadataOptions.HttpTokens"] == "required"
            self.instance, self.polls = "pending", 0
            return '<instancesSet><item><instanceId>i-web</instanceId><instanceState><name>pending</name></instanceState></item></instancesSet>'
        elif action == "DescribeInstances":
            assert p["InstanceId.1"] == "i-web"
            self.polls += 1
            if self.polls >= 2:
                self.instance = {"pending": "running", "stopping": "stopped", "shutting-down": "terminated"}.get(self.instance, self.instance)
            return f'<reservationSet><item><instancesSet><item><instanceId>i-web</instanceId><instanceState><name>{self.instance}</name></instanceState><ipAddress>203.0.113.42</ipAddress></item></instancesSet></item></reservationSet>'
        elif action == "StopInstances":
            assert self.instance == "running"
            assert "State" not in p, "semantic stopped-state input leaked into the AWS request"
            self.instance, self.polls = "stopping", 0
            return '<instancesSet><item><instanceId>i-web</instanceId><currentState><name>stopping</name></currentState></item></instancesSet>'
        elif action == "TerminateInstances":
            assert self.instance == "stopped", "terminate must follow observed stopped state"
            self.instance, self.polls = "shutting-down", 0
            return '<instancesSet><item><instanceId>i-web</instanceId><currentState><name>shutting-down</name></currentState></item></instancesSet>'
        elif action == "RevokeSecurityGroupIngress":
            assert self.instance in {None, "terminated"} and p["GroupId"] == "sg-web"
            assert p["IpPermissions.1.FromPort"] == "22" and p["IpPermissions.2.FromPort"] == "80"
            assert p["IpPermissions.1.IpRanges.1.CidrIp"] == "203.0.113.7/32"
            self.ingress = False
        elif action == "DeleteSecurityGroup":
            assert not self.ingress and self.instance in {None, "terminated"}
            self.group = False
        elif action == "DeleteKeyPair":
            assert self.instance in {None, "terminated"}
            self.key = False
        elif action == "DisassociateRouteTable":
            if p["AssociationId"] == "rtbassoc-subnet-a":
                assert self.instance in {None, "terminated"}
            self.associations.remove(p["AssociationId"])
        elif action == "DeleteRoute":
            assert self.instance in {None, "terminated"} and p["RouteTableId"] == "rtb-web" and p["DestinationCidrBlock"] == "0.0.0.0/0"
            self.route = False
        elif action == "DeleteRouteTable":
            assert not self.associations and not self.route
            self.routes = False
        elif action == "DetachInternetGateway":
            assert not self.route and self.instance in {None, "terminated"} and p["VpcId"] == "vpc-web"
            self.attached = False
        elif action == "DeleteInternetGateway":
            assert not self.attached
            self.gateway = False
        elif action == "DeleteSubnet":
            if p["SubnetId"] == "subnet-a":
                assert self.instance in {None, "terminated"}
            assert "rtbassoc-" + p["SubnetId"] not in self.associations
            self.subnets.remove(p["SubnetId"])
        elif action == "DeleteVpc":
            assert not self.subnets and not self.routes and not self.group and not self.attached
            self.vpc = False
        else:
            raise AssertionError(f"unexpected action {action}")
        return '<return>true</return>'

    def create_aws_nginx(self):
        self.cli("apply")
        self.assert_ledger_settled(13)
        identity = self.ledger_snapshot()["instance"]["identity"]
        assert base64.b64decode(identity).decode() == "i-web"
        assert self.instance == "running"
        assert len(self.subnets) == 2 and len(self.associations) == 2
        before = len(self.events)
        self.cli("reapply")
        assert all(e["action"].startswith("Describe") for e in self.events[before:]), "unchanged spec replayed a mutation"

    def inspect_aws_nginx(self):
        result = self.cli("status")
        row = json.loads(result.stdout)
        assert row["id"] == "i-web" and row["State"] == "running", row

    def destroy_aws_nginx(self):
        self.cli("stop")
        before = len(self.events)
        self.cli("stop")
        assert all(e["action"].startswith("Describe") for e in self.events[before:]), "unchanged stopped state replayed a mutation"
        self.cli("destroy")
        self.assert_ledger_settled(0)
        assert self.instance in {None, "terminated"}
        assert not any([self.vpc, self.gateway, self.attached, self.routes, self.route, self.group, self.ingress, self.key, self.subnets, self.associations])
        actions = [event["action"] for event in self.events]
        assert actions.count("RunInstances") == 1
        assert actions.index("StopInstances") < actions.index("TerminateInstances") < actions.index("DeleteVpc")
        (self.root / "api-transcript.json").write_text(json.dumps(self.events, indent=2))

    def lose_create_response_and_verify_ledger(self):
        self.lose_create_response = True
        self.cli("apply", expect_success=False)
        snapshot = self.ledger_snapshot()
        assert snapshot["instance"]["phase"] == 0 and not snapshot["instance"].get("identity"), snapshot
        assert self.instance == "pending", "mock must have created the remote VM"
        assert len([k for k in snapshot if not k.startswith(".")]) == 13, "compensation discarded dependencies of an uncertain create"
        actions = [event["action"] for event in self.events]
        assert actions.count("RunInstances") == 1, "transport retries duplicated an uncertain create"
        self.cli("apply", expect_success=False)
        assert [event["action"] for event in self.events] == actions, "recovery replayed a mutation"
        self.cli("destroy", "--skip-stop", expect_success=False)
        assert [event["action"] for event in self.events] == actions, "teardown deleted dependencies of an uncertain VM"
        retained = self.ledger_snapshot()
        assert retained["instance"]["phase"] == 0
        assert len([k for k in retained if not k.startswith(".")]) == 13

    def lose_delete_response_and_verify_ledger(self):
        self.cli("apply")
        self.cli("stop")
        self.lose_delete_response = True
        self.cli("destroy", expect_success=False)
        snapshot = self.ledger_snapshot()
        assert snapshot["instance"]["phase"] == 0
        assert base64.b64decode(snapshot["instance"]["identity"]).decode() == "i-web", "uncertain deletion lost prior identity"
        assert snapshot["instance"].get("about"), "uncertain deletion lost dependency metadata"
        assert self.vpc and self.group and "subnet-a" in self.subnets, "teardown deleted an uncertain VM's dependencies"
        actions = [event["action"] for event in self.events]
        assert actions.count("TerminateInstances") == 1, "transport retries duplicated an uncertain delete"
        self.cli("destroy", "--skip-stop", expect_success=False)
        assert [event["action"] for event in self.events] == actions, "restart blindly repeated delete"

    def interrupt_readiness_and_verify_recovery(self):
        self.pause_poll = True
        environment = {**__import__("os").environ, "AWS_ACCESS_KEY_ID": "MOCKACCESS", "AWS_SECRET_ACCESS_KEY": "mock-secret"}
        with (self.root / "interrupted.stdout.txt").open("w") as stdout, (self.root / "interrupted.stderr.txt").open("w") as stderr:
            process = subprocess.Popen(self.command("apply"), env=environment, stdout=stdout, stderr=stderr)
            try:
                assert self.poll_started.wait(20), f"readiness poll never started: {self.root}"
            finally:
                process.kill()
                process.wait(timeout=5)
                self.allow_poll.set()
        snapshot = self.ledger_snapshot()
        assert snapshot["instance"]["phase"] == 0 and not snapshot["instance"].get("identity")
        journal = [json.loads(line) for line in (self.root / "state/journal/create.jsonl").read_text().splitlines()]
        assert any(r.get("await_inputs", {}).get("InstanceId.1") == "i-web" for r in journal), "interruption lost polling handle"
        claims = json.loads(base64.b64decode(snapshot[".lease"]["proposed"]))["claims"]
        assert any("robot-nginx/instance" in c.get("keys", []) for c in claims), "interruption lost in-flight write lock"
        before = len(self.events)
        self.cli("destroy", "--skip-stop", expect_success=False)
        self.cli("apply", expect_success=False)
        assert len(self.events) == before, "restart wrote through an active lease or replayed create"

    def stop_aws_nginx_mock(self):
        if hasattr(self, "server"):
            self.server.shutdown()
            self.server.server_close()
            self.worker.join(timeout=5)
