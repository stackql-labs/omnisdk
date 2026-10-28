"""Build test-only caller configurations; no provider credentials or services are used."""
import hashlib
import json
import os


def write_caller_config(path, mechanism="bearer", memberships=True, permit=True,
                        known_identity=True):
    groups = ["readers", "operators"] if memberships else []
    subject = str(os.geteuid()) if mechanism == "os" else "robot-caller"
    config = {
        "issuer": "robot-local",
        "mechanism": mechanism,
        "directory": {
            "groups": groups,
            "principals": [{
                "id": "robot-user",
                "identities": [{"issuer": "robot-local",
                                "subject": subject if known_identity else "another-caller"}],
                "groups": groups,
            }],
        },
        "rules": [{"subject": "user:robot-user",
                   "resource": "command/omnicli/" + command,
                   "action": "execute"}
                  for command in ("whoami", "groups")] if permit else [],
    }
    if mechanism == "bearer":
        config["tokens"] = [{
            "subject": subject,
            "sha256": hashlib.sha256(b"robot-test-only-token").hexdigest(),
        }]
    with open(path, "w", encoding="utf-8") as output:
        json.dump(config, output)
