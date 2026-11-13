#!/usr/bin/env python3
"""Qualify chart egress in one owned namespace on an explicit test cluster.

Does not install a CNI, change cluster policy, or reuse application namespaces.
Run on a disposable cluster with an enforcing CNI and approved fixture image.
"""

import argparse
import json
import os
import subprocess
import uuid
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--context", required=True)
    parser.add_argument("--helm", default="helm")
    parser.add_argument("--image", required=True, help="Python fixture image available to the test cluster")
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    if os.environ.get("RUNKITE_TEST_EXECUTION_EGRESS") != "1":
        parser.error("set RUNKITE_TEST_EXECUTION_EGRESS=1 for this disposable-cluster qualification")
    namespace = "rk-egress-" + uuid.uuid4().hex[:16]
    created = False
    cases = []

    def kube(*arguments, data=None):
        return subprocess.run(
            ["kubectl", "--context", args.context, "--namespace", namespace, *arguments],
            input=data,
            text=True,
            capture_output=True,
            check=True,
            timeout=150,
        ).stdout

    def resource(value):
        kube("apply", "-f", "-", data=json.dumps(value))

    def pod(name, component):
        labels = {
            "app.kubernetes.io/name": "runkite",
            "app.kubernetes.io/instance": "egress-fixture",
            "app.kubernetes.io/component": component,
            "fixture": name,
        }
        containers = [{"name": "http", "image": args.image, "command": ["python", "-m", "http.server", "8080"]}]
        if name == "plane":
            containers.append(
                {"name": "grpc-port", "image": args.image, "command": ["python", "-m", "http.server", "50051"]}
            )
        if name == "runner":
            containers[0]["command"] = ["python", "-c", "import time;time.sleep(1800)"]
        resource(
            {
                "apiVersion": "v1",
                "kind": "Pod",
                "metadata": {"name": name, "labels": labels},
                "spec": {"automountServiceAccountToken": False, "containers": containers},
            }
        )

    def probe(source, target, port=8080):
        code = (
            "import urllib.request;urllib.request.urlopen(" + repr(f"http://{target}:{port}/") + ",timeout=3).read(1)"
        )
        try:
            kube("exec", source, "-c", "http", "--", "python", "-c", code)
            return True
        except subprocess.CalledProcessError:
            return False

    evidence = {"passed": False, "namespace": namespace, "cases": cases}
    try:
        kube("create", "namespace", namespace)
        created = True
        for name, component in [
            ("plane", "control-plane"),
            ("runner", "runner"),
            ("model", "model"),
            ("telemetry", "telemetry"),
            ("database", "database"),
            ("blocked", "blocked"),
        ]:
            pod(name, component)
        kube("wait", "--for=condition=Ready", "pod", "--all", "--timeout=120s")
        addresses = {
            name: kube("get", "pod", name, "-o", "jsonpath={.status.podIP}").strip()
            for name in ("plane", "model", "telemetry", "database", "blocked")
        }
        for name, address in addresses.items():
            if not probe("runner", address):
                raise AssertionError(f"baseline {name} was not reachable; no denial claim is valid")
        if not probe("runner", addresses["plane"], 50051) or not probe("plane", addresses["database"]):
            raise AssertionError("baseline plane/backend port was not reachable")
        cases.append("all fixture destinations reachable before applying policy")
        resource(
            {
                "apiVersion": "v1",
                "kind": "Service",
                "metadata": {"name": "plane"},
                "spec": {"selector": {"fixture": "plane"}, "ports": [{"port": 8080, "targetPort": 8080}]},
            }
        )
        command = [
            args.helm,
            "template",
            "egress-fixture",
            "deploy/helm/runkite",
            "--show-only",
            "templates/egress-networkpolicy.yaml",
            "--set",
            "secrets.existingSecret=unused-fixture",
            "--set",
            "runner.proxyOnly=true",
            "--set",
            "networkPolicy.egress.enabled=true",
            "--set",
            "config.httpPort=8080",
            "--set",
            "config.grpcPort=50051",
        ]
        for role, destinations in (("controlPlaneRules", ["database"]), ("runnerRules", ["model", "telemetry"])):
            for index, name in enumerate(destinations):
                command += [
                    "--set",
                    f"networkPolicy.egress.{role}[{index}].to[0].podSelector.matchLabels.fixture={name}",
                    "--set",
                    f"networkPolicy.egress.{role}[{index}].ports[0].port=8080",
                    "--set",
                    f"networkPolicy.egress.{role}[{index}].ports[0].protocol=TCP",
                ]
        rendered = subprocess.run(command, text=True, capture_output=True, check=True, timeout=30).stdout
        kube("apply", "-f", "-", data=rendered)
        # Require repeated observations rather than trusting API acceptance.
        import time

        deadline = time.monotonic() + 30
        while probe("runner", addresses["blocked"]):
            if time.monotonic() >= deadline:
                raise AssertionError("unlisted destination remained reachable; CNI did not enforce this policy")
            time.sleep(0.25)
        for name in ("plane", "model", "telemetry"):
            if not probe("runner", addresses[name]):
                raise AssertionError(f"required runner {name} egress was blocked")
            cases.append(f"runner {name} allowed")
        if not probe("runner", addresses["plane"], 50051):
            raise AssertionError("runner plane gRPC port was blocked")
        if probe("runner", addresses["database"]) or probe("plane", addresses["blocked"]):
            raise AssertionError("backend isolation or plane default-deny failed")
        if not probe("plane", addresses["database"]):
            raise AssertionError("configured plane backend was blocked")
        if not probe("runner", f"plane.{namespace}.svc.cluster.local"):
            raise AssertionError("required DNS path failed")
        cases += [
            "runner gRPC port allowed",
            "DNS and service resolution allowed",
            "runner database denied",
            "runner unlisted destination denied",
            "plane configured database allowed",
            "plane unlisted destination denied",
        ]
        evidence["passed"] = True
        evidence["not_qualified"] = [
            "actual cloud metadata service",
            "hostNetwork pods",
            "mutually hostile code in one worker",
            "live model/provider",
            "external OpenShell runtime",
        ]
    finally:
        if created:
            try:
                kube("delete", "namespace", namespace, "--wait=false")
            except subprocess.CalledProcessError:
                evidence["cleanup_requested"] = False
                evidence["passed"] = False
                raise
            evidence["cleanup_requested"] = True
        Path(args.output).write_text(json.dumps(evidence, indent=2) + "\n")


if __name__ == "__main__":
    main()
