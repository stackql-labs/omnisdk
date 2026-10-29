#!/usr/bin/env python3
"""Write isolated live lifecycle documents and staged intent; never call a cloud API."""
import argparse
import copy
import json
from pathlib import Path
import subprocess
import sys


def ref(path):
    return {"$ref": path}


def parameter(name, where="path"):
    return {"name": name, "in": where, "required": True, "schema": {"type": "string"}}


def operation(identifier, parameters, schema, body=None, code="200"):
    value = {"operationId": identifier, "parameters": parameters, "responses": {code: {"description": identifier}}}
    if schema:
        value["responses"][code]["content"] = {"application/json": {"schema": ref("#/components/schemas/" + schema)}}
    if body:
        value["requestBody"] = {"content": {"application/json": {"schema": ref("#/components/schemas/" + body)}}}
    return value


def method(path, verb, code="200", program=None):
    value = {"operation": ref("#/paths/" + path.replace("~", "~0").replace("/", "~1") + "/" + verb),
             "response": {"mediaType": "application/json", "openAPIDocKey": code}}
    if program:
        value["response"]["transform"] = {"type": "golang_template_json_v0.1.0", "body": program}
    return value


def google_document(profile):
    bucket = profile == "caller-name"
    base = "/b" if bucket else "/projects/{project}/global/networks"
    item = base + ("/{bucket}" if bucket else "/{network}")
    scope = [parameter("project", "query")] if bucket else [parameter("project")]
    addressed = [parameter("bucket")] if bucket else [parameter("project"), parameter("network")]
    string = {"type": "string"}
    props = {"name": string, "labels": {"type": "object", "additionalProperties": string}} if bucket else {
        "id": string, "name": string, "autoCreateSubnetworks": {"type": "boolean"},
        "routingConfig": {"type": "object", "properties": {"routingMode": string}}}
    schemas = {"Resource": {"type": "object", "properties": props}, "Operation": {"type": "object", "properties": {
        "name": string, "operation": string, "id": string, "status": string, "error": {"type": "object"}, "targetLink": string}}}
    read_program = None if bucket else '{"id":{{toJson .name}},"name":{{toJson .name}},"autoCreateSubnetworks":{{toJson .autoCreateSubnetworks}},"routingConfig":{"routingMode":{{toJson .routingConfig.routingMode}}}}'
    paths = {
        base: {"post": operation("create", scope, "Resource" if bucket else "Operation", "Resource")},
        item: {"get": operation("read", addressed, "Resource"),
               "patch": operation("update", addressed, "Resource" if bucket else "Operation", "Resource"),
               "delete": operation("delete", addressed, None if bucket else "Operation", code="204" if bucket else "200")}}
    methods = {"create": method(base, "post"), "read": method(item, "get", program=read_program),
               "update": method(item, "patch"), "delete": method(item, "delete", code="204" if bucket else "200")}
    change = "labels" if bucket else "routingConfig"
    if not bucket:
        poll = "/projects/{project}/global/operations/{operation}"
        paths[poll] = {"get": operation("poll", [parameter("project"), parameter("operation")], "Operation")}
        # The operation name is a polling handle, never the network identity.
        poll_program = '{"operation":{{toJson .name}},"status":{{toJson .status}},"error":{{toJson .error}}{{if .targetLink}},"id":{{toJson (getRegexpFirstMatch .targetLink "[^/]+$")}}{{end}}}'
        methods["poll"] = method(poll, "get", program=poll_program)
        for name in ["create", "update", "delete"]:
            early = profile == "await-eager"
            program = '{"operation":{{toJson .name}},"status":{{toJson .status}}'
            if early:
                program += '{{if .targetLink}},"id":{{toJson (getRegexpFirstMatch .targetLink "[^/]+$")}}{{end}}'
            program += '}'
            methods[name]["response"]["transform"] = {"type": "golang_template_json_v0.1.0", "body": program}
            methods[name]["x-stackQL-beta"] = {"method": ref("#/components/x-stackQL-resources/objects/methods/poll"),
                "bind": {"operation": "operation"}, "await": {"statusPath": "status", "done": "DONE", "errorPath": "error", "interval": "2s", "maxAttempts": 150}}
    resource = {"id": "lifecycle.cloud.objects", "name": "objects", "methods": methods,
                "updates": [{"priority": 100, "requires": [change], "permits": [change],
                             "target": ref("#/components/x-stackQL-resources/objects/methods/update")}],
                "sqlVerbs": {verb: [ref("#/components/x-stackQL-resources/objects/methods/" + name)]
                             for verb, name in [("select", "read"), ("insert", "create"), ("update", "update"), ("delete", "delete")]}}
    return {"openapi": "3.0.0", "info": {"title": "Identity lifecycle", "version": "1"},
            "servers": [{"url": "https://storage.googleapis.com/storage/v1" if bucket else "https://compute.googleapis.com/compute/v1"}],
            "paths": paths, "components": {"schemas": schemas, "x-stackQL-resources": {"objects": resource}}}


def write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n")


def prepare(args):
    root = args.out.resolve()
    root.mkdir(parents=True, exist_ok=False)
    bucket = args.profile == "caller-name"
    if args.profile == "aws":
        example = Path(__file__).resolve().parents[1] / "aws-vpc-subnet"
        subprocess.run([sys.executable, str(example / "prepare-update.py"), "--registry", str(args.registry), "--out", str(root / "registry")], check=True)
        resources = json.loads((example / "resources.json").read_text())["resources"]
        # Both tags are mutable ordinary intent in this walkthrough.
        for resource in resources:
            resource["desired"]["TagSpecification.1.Tag.1.Value"] = "initial"
            resource["write_once"].pop("TagSpecification.1.Tag.1.Value", None)
        args_data = {"params": {"region": args.region}}
    else:
        if not args.project or not args.location:
            raise ValueError("Google profiles require explicit --project and --location")
        provider = {"id": "lifecycle", "name": "lifecycle", "version": "v1", "providerServices": {
            "cloud": {"id": "cloud:v1", "name": "cloud", "preferred": True,
                      "service": ref("lifecycle/v1/services/cloud.yaml")}},
            "config": {"auth": {"type": "bearer", "credentialsenvvar": "OMNI_LIFECYCLE_TOKEN"}}}
        write(root / "registry/lifecycle/v1/provider.yaml", provider)
        write(root / "registry/lifecycle/v1/services/cloud.yaml", google_document(args.profile))
        resources = []
        for key in ["main", "secondary"]:
            name = f"{args.prefix}-{key}"
            desired = {"name": name, "labels": {"revision": "initial"}} if bucket else {
                "name": name, "autoCreateSubnetworks": False, "routingConfig": {"routingMode": "REGIONAL"}}
            if bucket:
                desired["location"] = args.location
            resources.append({"key": key, "provider": "lifecycle", "address": "cloud.objects", "identity": "name" if bucket else "id",
                              "addressed_by": "bucket" if bucket else "network", "desired": desired})
        if bucket:
            doc = google_document(args.profile)
            doc["components"]["schemas"]["Resource"]["properties"]["location"] = {"type": "string"}
            write(root / "registry/lifecycle/v1/services/cloud.yaml", doc)
        args_data = {"params": {"project": args.project}}
    def stage(label, revision, reduced=False):
        rs = copy.deepcopy(resources[:1] if reduced else resources)
        for resource in rs:
            if args.profile == "aws":
                resource["desired"]["TagSpecification.1.Tag.1.Value"] = revision
            elif bucket:
                resource["desired"]["labels"] = {"revision": revision}
            else:
                resource["desired"]["routingConfig"] = {"routingMode": "GLOBAL" if revision in ["one", "three", "after-prune"] else "REGIONAL"}
        write(root / (label + ".json"), {"args": args_data, "resources": rs})
    for label, revision in [("01-create", "initial"), ("02-update", "one"), ("03-update", "two"), ("04-update", "three")]:
        stage(label, revision)
    stage("05-after-prune", "after-prune" if args.profile != "await-eager" and args.profile != "await-late" else "two", True)
    local = json.loads((root / "05-after-prune.json").read_text())
    if args.profile == "aws":
        local["resources"][0]["desired"]["TagSpecification.1.Tag.1.Value"] = {"from": "input", "attribute": "public_digest"}
    elif bucket:
        local["resources"][0]["desired"]["labels"] = {"revision": "after-prune", "public_key": {"from": "input", "attribute": "public_digest"}}
    # Network updates use routingMode; the local key remains a caller-side artifact.
    write(root / "06-local.json", local)
    print(root)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", required=True, choices=["aws", "await-eager", "await-late", "caller-name"])
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--prefix", required=True, help="explicit unique resource-name prefix")
    parser.add_argument("--project")
    parser.add_argument("--location")
    parser.add_argument("--region")
    parser.add_argument("--registry", type=Path, default=Path("test/corpus/registry"))
    args = parser.parse_args()
    if args.profile == "aws" and not args.region:
        parser.error("AWS requires --region")
    prepare(args)
