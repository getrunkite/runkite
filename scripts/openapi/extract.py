#!/usr/bin/env python3
"""Extract METHOD+path pairs from Go source route registrations.

Parses:
  - internal/api/server.go: HandleFunc / Handle registrations
  - cmd/serve.go: GET /metrics, GET /metrics/, and mountPprof routes

Outputs a sorted list of "METHOD /path" lines to stdout, one per route.
Path params are normalized from Go camelCase ({agentID}) to snake_case
({agent_id}) for comparison with OpenAPI specs.
"""

import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent

# camelCase → snake_case for path param names
_CAMEL_RE = re.compile(r"([a-z0-9])([A-Z])")


def _camel_to_snake(name: str) -> str:
    return _CAMEL_RE.sub(r"\1_\2", name).lower()


def _normalize_path_params(path: str) -> str:
    """Convert {agentID} → {agent_id} etc."""
    return re.sub(r"\{(\w+)\}", lambda m: "{" + _camel_to_snake(m.group(1)) + "}", path)


# Patterns for Go 1.22+ method-prefixed HandleFunc/Handle registrations
# e.g.  mux.HandleFunc("GET /health", ...)
#        mux.Handle("GET /admin/", ...)
#        mux.HandleFunc("/custom/", ...)   -- no method prefix → all methods
_HANDLE_RE = re.compile(
    r'\.(?:HandleFunc|Handle)\(\s*"(?:(\w+)\s+)?(/[^"]*)"'
)


def _extract_from_file(filepath: Path) -> list[tuple[str, str]]:
    text = filepath.read_text()
    routes: list[tuple[str, str]] = []
    for m in _HANDLE_RE.finditer(text):
        method = m.group(1) or "*"
        path = m.group(2)
        path = _normalize_path_params(path)
        routes.append((method, path))
    return routes


def extract_routes() -> list[str]:
    """Return sorted list of 'METHOD /path' strings for every registered route."""
    server_go = REPO_ROOT / "internal" / "api" / "server.go"
    serve_go = REPO_ROOT / "cmd" / "serve.go"

    routes: set[str] = set()

    for filepath in (server_go, serve_go):
        if not filepath.exists():
            print(f"WARNING: {filepath} not found", file=sys.stderr)
            continue
        for method, path in _extract_from_file(filepath):
            if method == "*":
                routes.add(f"* {path}")
            else:
                routes.add(f"{method} {path}")

    return sorted(routes)


def main() -> None:
    routes = extract_routes()
    for r in routes:
        print(r)
    print(f"\n# Total: {len(routes)} routes", file=sys.stderr)


if __name__ == "__main__":
    main()
