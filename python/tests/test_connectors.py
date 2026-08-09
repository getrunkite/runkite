"""Self-check for connectors.py run-bound headers + MCP session token helpers.

Does not hit a live control plane -- uses httpx.MockTransport for network
behavior (401 re-mint / caller-supplied token no-retry).

Usage:
    python/.venv/bin/python python/tests/test_connectors.py
"""

from __future__ import annotations

import asyncio
import os
import sys
from unittest.mock import patch

import httpx

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.connectors import (  # noqa: E402
    HEADER_CONNECTOR_SESSION,
    ConnectorError,
    _run_bound_headers,
    proxy_connector_mcp,
)
from runkite_runner.tenant_ctx import HEADER_GENERATION, HEADER_RUN_ID, HEADER_TENANT_ID  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def test_requires_run_id():
    try:
        _run_bound_headers({"configurable": {}})
        check("missing run_id raises", False)
    except ValueError as e:
        check("missing run_id raises ValueError", "run_id" in str(e))


def test_headers_from_configurable():
    h = _run_bound_headers({"configurable": {"run_id": "run-1", "generation": 4}})
    check("run id header", h.get(HEADER_RUN_ID) == "run-1")
    check("generation header", h.get(HEADER_GENERATION) == "4")
    check("tenant header present", HEADER_TENANT_ID in h)


def test_accepts_bare_configurable_dict():
    h = _run_bound_headers({"run_id": "run-2", "generation": 1})
    check("bare configurable works", h.get(HEADER_RUN_ID) == "run-2")


def test_header_constant():
    check("HEADER_CONNECTOR_SESSION name", HEADER_CONNECTOR_SESSION == "X-Runkite-Connector-Session")


def _patch_async_client(transport: httpx.MockTransport):
    orig = httpx.AsyncClient

    def factory(*args, **kwargs):
        kwargs["transport"] = transport
        return orig(*args, **kwargs)

    return patch("runkite_runner.connectors.httpx.AsyncClient", side_effect=factory)


def test_proxy_remints_once_on_401():
    counts = {"session": 0, "mcp": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if path.endswith("/session"):
            counts["session"] += 1
            return httpx.Response(200, json={"session_token": f"tok-{counts['session']}", "mcp": {"url": "/x"}})
        if path.endswith("/mcp"):
            counts["mcp"] += 1
            if counts["mcp"] == 1:
                return httpx.Response(401, text="expired")
            return httpx.Response(200, json={"jsonrpc": "2.0", "id": 1, "result": {}})
        return httpx.Response(404)

    transport = httpx.MockTransport(handler)
    with _patch_async_client(transport):
        result = asyncio.run(
            proxy_connector_mcp(
                {"configurable": {"run_id": "run-1", "generation": 1}},
                "sf",
                {"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
                control_plane_url="http://example.invalid",
            )
        )
    check("401 remint returns result", result.get("result") == {})
    # session → mcp-401 → re-mint session → mcp-success
    check("401 remint session calls", counts["session"] == 2)
    check("401 remint mcp calls", counts["mcp"] == 2)


def test_proxy_no_retry_when_caller_supplies_token():
    counts = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        counts["n"] += 1
        return httpx.Response(401, text="expired")

    transport = httpx.MockTransport(handler)
    with _patch_async_client(transport):
        try:
            asyncio.run(
                proxy_connector_mcp(
                    {"configurable": {"run_id": "run-1", "generation": 1}},
                    "sf",
                    {"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
                    session_token="caller-tok",
                    control_plane_url="http://example.invalid",
                )
            )
            check("caller token 401 raises", False)
        except ConnectorError as e:
            check("caller token 401 raises ConnectorError", "401" in str(e))
    check("caller token single call", counts["n"] == 1)


def main():
    test_requires_run_id()
    test_headers_from_configurable()
    test_accepts_bare_configurable_dict()
    test_header_constant()
    test_proxy_remints_once_on_401()
    test_proxy_no_retry_when_caller_supplies_token()
    # ConnectorError is part of the public surface -- keep the import live.
    check("ConnectorError is an Exception", issubclass(ConnectorError, Exception))
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
