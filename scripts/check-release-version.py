#!/usr/bin/env python3
"""Check coordinated release metadata using Python 3.11+ standard libraries."""

import argparse
import ast
import json
import os
import re
import sys
from pathlib import Path

import tomllib

ROOT = Path(__file__).resolve().parents[1]
NUMBER = r"(?:0|[1-9][0-9]*)"
PRERELEASE = rf"(?:{NUMBER}|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
BUILD = r"[0-9A-Za-z-]+"
VERSION_PATTERN = re.compile(
    rf"{NUMBER}\.{NUMBER}\.{NUMBER}(?:-{PRERELEASE}(?:\.{PRERELEASE})*)?(?:\+{BUILD}(?:\.{BUILD})*)?"
)
PACKAGES = {"runkite-runner": "typescript/runkite-runner", "runkite-governance": "typescript/runkite-governance"}


def python_version(path):
    tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
    values = []
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(
            isinstance(target, ast.Name) and target.id == "__version__" for target in node.targets
        ):
            values.append(ast.literal_eval(node.value))
    if len(values) != 1 or not isinstance(values[0], str):
        raise ValueError("expected one literal __version__ assignment")
    return values[0]


def chart_versions(path):
    fields = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        match = re.match(r"^(version|appVersion):\s*(.*)$", line)
        if not match:
            continue
        key, value = match.groups()
        if key in fields:
            raise ValueError(f"duplicate {key}")
        value = value.split("#", 1)[0].strip()
        if value.startswith(('"', "'")):
            if len(value) < 2 or value[-1] != value[0]:
                raise ValueError(f"invalid quoted {key}")
            value = value[1:-1]
        fields[key] = value
    return fields


def example_manifests(root):
    for directory, children, files in os.walk(root / "examples"):
        children[:] = sorted(name for name in children if name not in {"node_modules", ".venv", ".git"})
        if "package.json" in files:
            yield Path(directory) / "package.json"


def check_release_versions(root, tag=None):
    root = root.resolve()
    errors = []
    try:
        version = (root / "VERSION").read_text(encoding="utf-8").strip()
        if not VERSION_PATTERN.fullmatch(version):
            raise ValueError("expected one semantic version")
    except (OSError, ValueError) as error:
        return [f"VERSION: {error}"]

    def compare(path, field, value):
        if value != version:
            errors.append(f"{path.relative_to(root)}:{field}: expected {version!r}, found {value!r}")

    def read_json(path):
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
            if not isinstance(value, dict):
                raise ValueError("expected a JSON object")
            return value
        except (OSError, ValueError) as error:
            errors.append(f"{path.relative_to(root)}: {error}")
            return {}

    if tag is not None and tag != "v" + version:
        errors.append(f"release tag: expected {'v' + version!r}, found {tag!r}")
    init = root / "python/runkite_runner/__init__.py"
    try:
        compare(init, "__version__", python_version(init))
    except (OSError, ValueError, SyntaxError) as error:
        errors.append(f"{init.relative_to(root)}: {error}")
    for relative in ["python/pyproject.toml", "python/runkite_governance/pyproject.toml"]:
        path = root / relative
        try:
            with path.open("rb") as stream:
                compare(path, "project.version", tomllib.load(stream).get("project", {}).get("version"))
        except (OSError, ValueError) as error:
            errors.append(f"{relative}: {error}")
    package_directories = {(root / relative).resolve(): name for name, relative in PACKAGES.items()}
    for name, relative in PACKAGES.items():
        manifest_path = root / relative / "package.json"
        manifest = read_json(manifest_path)
        compare(manifest_path, "version", manifest.get("version"))
        if manifest.get("name") != name:
            errors.append(f"{relative}/package.json:name: expected {name!r}")
        lock_path = root / relative / "package-lock.json"
        lock = read_json(lock_path)
        compare(lock_path, "version", lock.get("version"))
        compare(lock_path, "packages[''].version", lock.get("packages", {}).get("", {}).get("version"))
    chart = root / "deploy/helm/runkite/Chart.yaml"
    try:
        fields = chart_versions(chart)
        for key in ["version", "appVersion"]:
            compare(chart, key, fields.get(key))
    except (OSError, ValueError) as error:
        errors.append(f"{chart.relative_to(root)}: {error}")

    # Only snapshots of coordinated file-linked packages are release metadata.
    # Private example versions and unrelated dependency versions stay independent.
    for manifest_path in example_manifests(root):
        manifest = read_json(manifest_path)
        linked = set()
        for section in ["dependencies", "devDependencies", "optionalDependencies"]:
            for value in manifest.get(section, {}).values():
                if isinstance(value, str) and value.startswith("file:"):
                    target = (manifest_path.parent / value[5:]).resolve()
                    if target in package_directories:
                        linked.add(target)
        lock_path = manifest_path.with_name("package-lock.json")
        if not lock_path.is_file() and not linked:
            continue
        lock = read_json(lock_path)
        found = set()
        for key, snapshot in lock.get("packages", {}).items():
            if not key or key.startswith("node_modules/"):
                continue
            target = (manifest_path.parent / key).resolve()
            if target in package_directories:
                found.add(target)
                compare(lock_path, f"packages[{key!r}].version", snapshot.get("version"))
        for target in sorted(linked - found):
            errors.append(
                f"{lock_path.relative_to(root)}: missing file-linked snapshot for {package_directories[target]}"
            )
    return errors


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", help="Require this exact vVERSION release tag before publication")
    args = parser.parse_args()
    errors = check_release_versions(ROOT, args.tag)
    if errors:
        print("Release metadata mismatch:", file=sys.stderr)
        for error in errors:
            print(f"  {error}", file=sys.stderr)
        return 1
    print(f"Release metadata matches VERSION {(ROOT / 'VERSION').read_text().strip()}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
