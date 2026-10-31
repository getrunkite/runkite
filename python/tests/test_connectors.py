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
    get_connector_session,
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
                return httpx.Response(401, json={"reason_code": "connector_session_invalid"})
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
        return httpx.Response(401, json={"reason_code": "connector_session_invalid"})

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


async def test_total_deadline_and_cancellation():
    counts = {"n": 0}

    async def slow_phases(request):
        counts["n"] += 1
        await asyncio.sleep(0.025)
        if request.url.path.endswith("/session"):
            return httpx.Response(200, json={"session_token": "token"})
        return httpx.Response(401, json={"reason_code": "connector_session_invalid"})

    with _patch_async_client(httpx.MockTransport(slow_phases)):
        try:
            await proxy_connector_mcp({"run_id": "r"}, "sf", {"method": "tools/call"}, timeout=0.06)
            check("total deadline expires across renewal", False)
        except TimeoutError:
            check("renewal shares the original deadline", counts["n"] < 4)

    clients = []
    entered = asyncio.Event()
    original = httpx.AsyncClient

    async def never_finishes(_request):
        entered.set()
        await asyncio.Event().wait()

    def client_factory(*args, **kwargs):
        kwargs["transport"] = httpx.MockTransport(never_finishes)
        client = original(*args, **kwargs)
        clients.append(client)
        return client

    with patch("runkite_runner.connectors.httpx.AsyncClient", side_effect=client_factory):
        task = asyncio.create_task(proxy_connector_mcp({"run_id": "r"}, "sf", {}, session_token="token"))
        await asyncio.wait_for(entered.wait(), 1)
        task.cancel()
        try:
            await task
            check("task cancellation propagates", False)
        except asyncio.CancelledError:
            check("cancel closes the active client", all(client.is_closed for client in clients))


async def test_body_deadline_and_unmarked_401():
    class SlowBody(httpx.AsyncByteStream):
        closed = False

        async def __aiter__(self):
            while True:
                await asyncio.sleep(0.005)
                yield b" "

        async def aclose(self):
            self.closed = True

    body = SlowBody()
    with _patch_async_client(httpx.MockTransport(lambda _request: httpx.Response(200, stream=body))):
        try:
            await get_connector_session({"run_id": "r"}, "sf", timeout=0.025)
            check("dripping body is bounded", False)
        except TimeoutError:
            check("timeout closes a dripping body", body.closed)

    calls = []

    def unauthorized(request):
        calls.append(request)
        if request.url.path.endswith("/session"):
            return httpx.Response(200, json={"session_token": "token"})
        return httpx.Response(401, text="upstream unauthorized after operation")

    with _patch_async_client(httpx.MockTransport(unauthorized)):
        try:
            await proxy_connector_mcp({"run_id": "r"}, "sf", {"method": "tools/call"})
            check("unmarked 401 fails", False)
        except ConnectorError:
            check("unmarked 401 never replays upstream", len(calls) == 2)


async def test_snapshot_encoding_and_invalid_arguments():
    config = {"run_id": "original", "generation": 3}
    body = {"method": "tools/list", "text": "你好"}
    calls = []

    def snapshot(request):
        calls.append(request)
        if request.url.path.endswith("/session"):
            config.update(run_id="replacement", generation=4)
            body["method"] = "tools/call"
            return httpx.Response(200, json={"session_token": "token"})
        return httpx.Response(200, json={"result": {}})

    with _patch_async_client(httpx.MockTransport(snapshot)):
        await proxy_connector_mcp(config, "sf/with?query#fragment", body)
        check("connector name occupies one encoded segment", b"sf%2Fwith%3Fquery%23fragment" in calls[0].url.raw_path)
        check("run binding snapshot preserved", calls[-1].headers[HEADER_RUN_ID] == "original")
        check("generation snapshot preserved", calls[-1].headers[HEADER_GENERATION] == "3")
        import json

        check("MCP request snapshot preserved", json.loads(calls[-1].content)["method"] == "tools/list")
        check("request retains compact UTF-8 wire encoding", "你好".encode() in calls[-1].content)
        before = len(calls)
        for timeout in (0, -1, float("nan"), float("inf"), True, 10**1000):
            try:
                await get_connector_session({"run_id": "r"}, "sf", timeout=timeout)
                check("invalid timeout fails", False)
            except ValueError:
                pass
        for name in ("", ".", ".."):
            try:
                await get_connector_session({"run_id": "r"}, name)
                check("invalid connector name fails", False)
            except ValueError:
                pass
        check("invalid arguments never make HTTP requests", len(calls) == before)


async def test_non_success_cannot_become_connector_result():
    transport = httpx.MockTransport(
        lambda _request: httpx.Response(302, json={"session_token": "not-authorized", "result": {}})
    )
    with _patch_async_client(transport):
        for operation in (
            get_connector_session({"run_id": "r"}, "sf"),
            proxy_connector_mcp({"run_id": "r"}, "sf", {}, session_token="token"),
        ):
            try:
                await operation
                check("redirect status is not a connector success", False)
            except ConnectorError as exc:
                check("redirect status is not a connector success", "HTTP 302" in str(exc))


def main():
    test_requires_run_id()
    test_headers_from_configurable()
    test_accepts_bare_configurable_dict()
    test_header_constant()
    test_proxy_remints_once_on_401()
    test_proxy_no_retry_when_caller_supplies_token()
    asyncio.run(test_non_success_cannot_become_connector_result())
    asyncio.run(test_total_deadline_and_cancellation())
    asyncio.run(test_body_deadline_and_unmarked_401())
    asyncio.run(test_snapshot_encoding_and_invalid_arguments())
    # ConnectorError is part of the public surface -- keep the import live.
    check("ConnectorError is an Exception", issubclass(ConnectorError, Exception))
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
