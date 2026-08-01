#!/usr/bin/env python3
"""Verify every Go route extracted by extract.py appears in the appropriate
OpenAPI spec (after path-param normalization).

Exit 0 if complete; exit 1 with a missing-route listing otherwise.
"""

import json
import re
import sys
from pathlib import Path

# Allow importing extract from the same directory
sys.path.insert(0, str(Path(__file__).resolve().parent))
from extract import extract_routes  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
SPEC_DIR = REPO_ROOT / "spec"


def _spec_methods_paths(spec: dict) -> set[str]:
    """Return a set of 'METHOD /path' from an OpenAPI spec."""
    result: set[str] = set()
    for path, item in spec.get("paths", {}).items():
        for method in ("get", "put", "post", "delete", "patch", "head", "options", "trace"):
            if method in item:
                result.add(f"{method.upper()} {path}")
    return result


def _load_spec(name: str) -> dict:
    p = SPEC_DIR / name
    if not p.exists():
        print(f"ERROR: {p} not found — run `python3 scripts/openapi/build.py` first", file=sys.stderr)
        sys.exit(1)
    return json.loads(p.read_text())


def _collect_refs(obj, acc: set[str] | None = None) -> set[str]:
    if acc is None:
        acc = set()
    if isinstance(obj, dict):
        if "$ref" in obj and isinstance(obj["$ref"], str):
            acc.add(obj["$ref"])
        for v in obj.values():
            _collect_refs(v, acc)
    elif isinstance(obj, list):
        for v in obj:
            _collect_refs(v, acc)
    return acc


def _check_spec_integrity(name: str, spec: dict) -> list[str]:
    """Return human-readable integrity problems (duplicate operationIds, broken $refs)."""
    problems: list[str] = []
    seen: dict[str, str] = {}
    for path, item in spec.get("paths", {}).items():
        for method, op in item.items():
            if method not in ("get", "put", "post", "delete", "patch", "head", "options", "trace"):
                continue
            if not isinstance(op, dict):
                continue
            oid = op.get("operationId")
            if not oid:
                problems.append(f"{name}: {method.upper()} {path} missing operationId")
                continue
            prev = seen.get(oid)
            if prev:
                problems.append(f"{name}: duplicate operationId {oid!r} ({prev} and {method.upper()} {path})")
            else:
                seen[oid] = f"{method.upper()} {path}"

    schemas = set(spec.get("components", {}).get("schemas", {}))
    responses = set(spec.get("components", {}).get("responses", {}))
    for ref in _collect_refs(spec):
        if ref.startswith("#/components/schemas/"):
            n = ref.rsplit("/", 1)[-1]
            if n not in schemas:
                problems.append(f"{name}: broken $ref {ref}")
        elif ref.startswith("#/components/responses/"):
            n = ref.rsplit("/", 1)[-1]
            if n not in responses:
                problems.append(f"{name}: broken $ref {ref}")
    return problems


def main() -> None:
    public = _load_spec("openapi.json")
    admin = _load_spec("openapi-admin.json")
    internal = _load_spec("openapi-internal.json")

    pub_routes = _spec_methods_paths(public)
    admin_routes = _spec_methods_paths(admin)
    internal_routes = _spec_methods_paths(internal)
    all_spec_routes = pub_routes | admin_routes | internal_routes

    go_routes = extract_routes()
    missing: list[str] = []

    for route in go_routes:
        parts = route.split(" ", 1)
        method, path = parts[0], parts[1]

        if method == "*":
            # Wildcard (Handle without method prefix): check that at least
            # one method for this path exists in any spec. The catch-all
            # "/" handler is the mux fallback, not a real route to document.
            if path == "/":
                continue
            found = any(
                r.endswith(f" {path}") for r in all_spec_routes
            )
            if not found:
                missing.append(route)
            continue

        key = f"{method} {path}"
        if key not in all_spec_routes:
            missing.append(route)

    integrity: list[str] = []
    for name, spec in (
        ("openapi.json", public),
        ("openapi-admin.json", admin),
        ("openapi-internal.json", internal),
    ):
        integrity.extend(_check_spec_integrity(name, spec))

    if missing or integrity:
        if missing:
            print(f"FAIL: {len(missing)} Go route(s) not found in any OpenAPI spec:\n", file=sys.stderr)
            for r in missing:
                print(f"  {r}", file=sys.stderr)
        if integrity:
            print(f"FAIL: {len(integrity)} OpenAPI integrity issue(s):\n", file=sys.stderr)
            for p in integrity:
                print(f"  {p}", file=sys.stderr)
        print(f"\nPublic spec ops : {len(pub_routes)}", file=sys.stderr)
        print(f"Admin spec ops  : {len(admin_routes)}", file=sys.stderr)
        print(f"Internal spec ops: {len(internal_routes)}", file=sys.stderr)
        sys.exit(1)
    else:
        print(f"OK: all {len(go_routes)} Go routes covered; specs integrity clean")
        print(f"  Public spec   : {len(pub_routes)} ops")
        print(f"  Admin spec    : {len(admin_routes)} ops")
        print(f"  Internal spec : {len(internal_routes)} ops")


if __name__ == "__main__":
    main()
