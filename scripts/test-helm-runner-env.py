"""Render scalar and Secret-backed runner environment entries with Helm."""

import json
import os
import re
import subprocess
import tempfile
from pathlib import Path

root = Path(__file__).resolve().parents[1]
helm = os.environ.get("HELM", "helm")
env = {
    "LOG_LEVEL": "info",
    "ZERO": 0,
    "MODEL_API_KEY": {"valueFrom": {"secretKeyRef": {"name": "model-creds", "key": "key"}}},
}
with tempfile.NamedTemporaryFile(mode="w", suffix=".json") as values:
    json.dump({"secrets": {"existingSecret": "env-test-creds"}, "runner": {"extraEnv": env}}, values)
    values.flush()
    result = subprocess.run(
        [helm, "template", "env-test", str(root / "deploy/helm/runkite"), "-f", values.name],
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stderr
    assert re.search(r'name: "LOG_LEVEL"\s+value: "info"', result.stdout)
    assert re.search(r'name: "ZERO"\s+value: "0"', result.stdout)
    assert re.search(r'name: "MODEL_API_KEY"\s+valueFrom:\s+secretKeyRef:', result.stdout)
    assert "name: model-creds" in result.stdout
    values.seek(0)
    values.truncate()
    json.dump(
        {
            "secrets": {"existingSecret": "env-test-creds"},
            "runner": {"extraEnv": {"BAD": {"value": "x", "valueFrom": {"fieldRef": {"fieldPath": "metadata.name"}}}}},
        },
        values,
    )
    values.flush()
    invalid = subprocess.run(
        [helm, "template", "env-test", str(root / "deploy/helm/runkite"), "-f", values.name],
        capture_output=True,
        text=True,
    )
    assert invalid.returncode != 0, "ambiguous environment source was accepted"
print("Runner environment render and validation passed")
