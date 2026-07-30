"""Self-check for the Agent-to-Agent (A2A) delegation client
(python/runkite_runner/a2a.py). Uses httpx.MockTransport (native to
httpx, no extra mocking dependency) to inspect the actual outgoing
request without a live control plane -- live end-to-end verification
(real control plane, real runner, two real agents) is covered
separately, this just proves call_agent builds the request correctly.

Usage:
    python/.venv/bin/python python/tests/test_a2a.py
"""

import asyncio
import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import httpx  # noqa: E402
from runkite_runner.a2a import A2AError, call_agent  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


class _FakeUser:
    """Matches RunnerUser's to_dict() shape without needing a full
    RunnerUser instance (factory_graph.py's own tests cover that)."""

    def __init__(self, data):
        self._data = data

    def to_dict(self):
        return dict(self._data)


async def test_missing_run_id_raises():
    try:
        await call_agent({"configurable": {}}, "other_agent", {"messages": []})
        check("missing run_id raises RuntimeError", False)
    except RuntimeError:
        check("missing run_id raises RuntimeError", True)


async def test_builds_correct_request_body_and_headers():
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["url"] = str(request.url)
        captured["headers"] = dict(request.headers)
        captured["body"] = json.loads(request.content)
        return httpx.Response(200, json={"run_id": "child-run", "status": "pending"})

    transport = httpx.MockTransport(handler)

    async def _fake_client(*args, **kwargs):
        kwargs["transport"] = transport
        return httpx.AsyncClient(*args, **kwargs)

    # Patch at the module level so call_agent's own httpx.AsyncClient(...)
    # construction picks up the mock transport.
    import runkite_runner.a2a as a2a_module

    original_client = httpx.AsyncClient
    a2a_module.httpx.AsyncClient = lambda *a, **kw: original_client(*a, transport=transport, **kw)

    os.environ["RUNNER_TOKEN"] = "test-token"
    try:
        config = {
            "configurable": {
                "run_id": "parent-run-123",
                "langgraph_auth_user": _FakeUser({"identity": "alice", "email": "alice@example.com"}),
            }
        }
        result = await call_agent(
            config,
            "worker_agent",
            {"messages": [{"role": "human", "content": "do the thing"}]},
            wait=False,
            control_plane_url="http://fake-control-plane:2026",
        )
    finally:
        a2a_module.httpx.AsyncClient = original_client
        del os.environ["RUNNER_TOKEN"]

    check("posted to the internal a2a endpoint", captured["url"] == "http://fake-control-plane:2026/internal/a2a/runs")
    check("body has correct agent_id", captured["body"]["agent_id"] == "worker_agent")
    check("body has correct parent_run_id (from config)", captured["body"]["parent_run_id"] == "parent-run-123")
    check("body has wait=false", captured["body"]["wait"] is False)
    check(
        "body forwards on_behalf_of from langgraph_auth_user.to_dict()",
        captured["body"]["on_behalf_of"] == {"identity": "alice", "email": "alice@example.com"},
    )
    check("runner auth headers set from RUNNER_TOKEN", captured["headers"].get("x-runner-kind") == "python-langgraph")
    check("runner token header set", captured["headers"].get("x-runner-token") == "test-token")
    check("returns the parsed response", result == {"run_id": "child-run", "status": "pending"})


async def test_no_user_in_config_omits_on_behalf_of():
    captured = {}

    def handler(request: httpx.Request) -> httpx.Response:
        captured["body"] = json.loads(request.content)
        return httpx.Response(200, json={"run_id": "child-run"})

    transport = httpx.MockTransport(handler)
    import runkite_runner.a2a as a2a_module

    original_client = httpx.AsyncClient
    a2a_module.httpx.AsyncClient = lambda *a, **kw: original_client(*a, transport=transport, **kw)

    try:
        config = {"configurable": {"run_id": "parent-run-456"}}
        await call_agent(config, "worker_agent", {}, wait=False)
    finally:
        a2a_module.httpx.AsyncClient = original_client

    check("on_behalf_of omitted when no authenticated user in config", "on_behalf_of" not in captured["body"])


async def test_error_response_raises_a2a_error():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(400, text='{"message":"a2a delegation depth 11 exceeds max_depth 10"}')

    transport = httpx.MockTransport(handler)
    import runkite_runner.a2a as a2a_module

    original_client = httpx.AsyncClient
    a2a_module.httpx.AsyncClient = lambda *a, **kw: original_client(*a, transport=transport, **kw)

    try:
        config = {"configurable": {"run_id": "parent-run"}}
        try:
            await call_agent(config, "worker_agent", {}, wait=False)
            check("400 response raises A2AError", False)
        except A2AError as e:
            check("400 response raises A2AError", "depth" in str(e))
    finally:
        a2a_module.httpx.AsyncClient = original_client


def main():
    asyncio.run(test_missing_run_id_raises())
    asyncio.run(test_builds_correct_request_body_and_headers())
    asyncio.run(test_no_user_in_config_omits_on_behalf_of())
    asyncio.run(test_error_response_raises_a2a_error())
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
