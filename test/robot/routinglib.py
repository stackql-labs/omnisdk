"""Small declared routing bundle; its targets must never be executed by discovery."""
from pathlib import Path


def write_routing_bundle(root, invalid_target=False, invalid_attributes=False):
    root = Path(root)
    root.mkdir(parents=True, exist_ok=True)
    (root / "provider.yaml").write_text(
        "name: demo\nproviderServices:\n  routing:\n    service: {$ref: routing.yaml}\n",
        encoding="utf-8")
    document = """openapi: 3.0.0
updates:
  - resources: {$ref: '#/components/x-stackQL-resources/widgets'}
    priority: 20
    permits: [name, tags]
    target: '#/components/x-stackQL-resources/widgets/methods/low'
components:
  x-stackQL-resources:
    widgets:
      updates:
        - priority: 100
          requires: [name]
          permits: [name, tags]
          target: {$ref: '#/components/x-stackQL-resources/widgets/methods/high'}
        - priority: 20
          permits: [name]
          target: '#/components/x-stackQL-resources/widgets/methods/middle'
      methods:
        high:
          updates:
            - priority: 40
              requires: [name]
              permits: [name]
              target: '#/components/x-stackQL-resources/widgets/methods/high'
          inline: [omni-routing-discovery-must-not-execute-this]
        low:
          inline: [omni-routing-discovery-must-not-execute-this]
        middle:
          inline: [omni-routing-discovery-must-not-execute-this]
"""
    if invalid_target:
        document = document.replace("/methods/high", "/methods/missing")
    if invalid_attributes:
        document = document.replace("requires: [name]", "requires: [undeclared]")
    (root / "routing.yaml").write_text(document, encoding="utf-8")
