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
                try:
                    with owner.lock:
                        owner.events.append({"action": action, "params": values})
                        fields = owner.dispatch(action, values)
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
        result = self.cli("prepare", "--registry", str(self.repo / "test/corpus/registry"),
                          "--image-id", "ami-00000000000000001", "--zone-a", "ap-southeast-2a",
                          "--zone-b", "ap-southeast-2b", "--ssh-cidr", "203.0.113.7/32",
                          "--prefix", "robot-nginx", "--poll-interval", "5ms", "--poll-attempts", "10")
        assert not self.events, "preparation contacted AWS"
        assert (self.root / "ec2.merge.json").exists(), result.stdout

    def cli(self, command, *arguments):
        result = subprocess.run([self.binary, command, "--case", str(self.root), *arguments,
                                 *([] if command == "prepare" else ["--endpoint", self.endpoint])],
                                capture_output=True, text=True, timeout=60,
                                env={**__import__("os").environ, "AWS_ACCESS_KEY_ID": "MOCKACCESS", "AWS_SECRET_ACCESS_KEY": "mock-secret"})
        (self.root / f"{command}.stdout.txt").write_text(result.stdout)
        (self.root / f"{command}.stderr.txt").write_text(result.stderr)
        with self.lock:
            (self.root / "api-transcript.json").write_text(json.dumps(self.events, indent=2))
        assert result.returncode == 0, f"{command}: {result.stdout}\n{result.stderr}\ncase: {self.root}"
        return result

    def dispatch(self, action, p):
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
        outputs = json.loads((self.root / "outputs.json").read_text())
        assert outputs["instance_id"] == "i-web", outputs
        assert self.instance == "running"
        assert len(self.subnets) == 2 and len(self.associations) == 2

    def inspect_aws_nginx(self):
        result = self.cli("status")
        row = json.loads(result.stdout)
        assert row["id"] == "i-web" and row["state"] == "running", row

    def destroy_aws_nginx(self):
        self.cli("destroy")
        assert self.instance in {None, "terminated"}
        assert not any([self.vpc, self.gateway, self.attached, self.routes, self.route, self.group, self.ingress, self.key, self.subnets, self.associations])
        actions = [event["action"] for event in self.events]
        assert actions.count("RunInstances") == 1
        assert actions.index("StopInstances") < actions.index("TerminateInstances") < actions.index("DeleteVpc")
        (self.root / "api-transcript.json").write_text(json.dumps(self.events, indent=2))

    def stop_aws_nginx_mock(self):
        if hasattr(self, "server"):
            self.server.shutdown()
            self.server.server_close()
            self.worker.join(timeout=5)
