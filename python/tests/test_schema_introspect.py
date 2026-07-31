"""Self-check for schema_introspect.py: real JSON Schema extraction from a
compiled LangGraph graph, and reporting it to the control plane's
PUT /internal/agents/{agentID}/schema (see internal/api/agents.go's
handleReportAgentSchema).

Proves:
1. extract_agent_schema() pulls a REAL schema (with actual field names/
   types) from a real compiled StateGraph, not the {"type":"object"}
   stub -- input/output/state/config all populated.
2. A field extraction that fails falls back to {"type":"object"} for
   just that field, without failing the other three.
3. report_agent_schemas() PUTs the introspected schema for every STATIC
   graph to the right URL with the right runner-token headers, and
   silently skips a graph whose report fails (doesn't raise, doesn't
   block the other graphs).

Usage:
    python/.venv/bin/python python/tests/test_schema_introspect.py
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
from typing import TypedDict

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import httpx  # noqa: E402
from langgraph.graph import END, START, StateGraph  # noqa: E402
from runkite_runner.schema_introspect import extract_agent_schema, report_agent_schemas  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


class MyState(TypedDict):
    messages: list
    count: int


def _node(state):
    return {"count": state.get("count", 0) + 1}


def _build_graph():
    g = StateGraph(MyState)
    g.add_node("n", _node)
    g.add_edge(START, "n")
    g.add_edge("n", END)
    return g.compile()


def test_extract_agent_schema_pulls_real_field_names():
    graph = _build_graph()
    schema = extract_agent_schema(graph)

    check("input_schema is populated", schema["input_schema"] != {})
    props = schema["input_schema"].get("properties", {})
    check("input_schema has the real 'messages' field", "messages" in props)
    check("input_schema has the real 'count' field", "count" in props)
    check(
        "output_schema also has real fields (same shape here)", "count" in schema["output_schema"].get("properties", {})
    )
    check("state_schema is populated with the real title", schema["state_schema"].get("title") == "MyState")
    check(
        "config_schema is a dict (even if empty for a graph with no configurable fields)",
        isinstance(schema["config_schema"], dict),
    )


def test_extract_agent_schema_field_failure_falls_back_independently():
    class BrokenGraph:
        def get_input_jsonschema(self):
            raise RuntimeError("boom")

        def get_output_jsonschema(self):
            return {"type": "object", "title": "RealOutput"}

        def get_config_jsonschema(self):
            raise RuntimeError("boom")

        class builder:
            pass  # no state_schema attribute at all -> AttributeError

    schema = extract_agent_schema(BrokenGraph())
    check("failed input extraction falls back to the stub", schema["input_schema"] == {"type": "object"})
    check(
        "successful output extraction is NOT affected by input's failure",
        schema["output_schema"].get("title") == "RealOutput",
    )
    check("failed config extraction falls back to the stub", schema["config_schema"] == {"type": "object"})
    check("failed state extraction falls back to the stub", schema["state_schema"] == {"type": "object"})


async def test_report_agent_schemas_puts_real_schema_with_auth_headers():
    captured = []

    def handler(request: httpx.Request) -> httpx.Response:
        captured.append(
            {
                "method": request.method,
                "url": str(request.url),
                "headers": dict(request.headers),
                "body": json.loads(request.content),
            }
        )
        return httpx.Response(204)

    transport = httpx.MockTransport(handler)
    import runkite_runner.schema_introspect as si_module

    original_client = httpx.AsyncClient
    si_module.httpx.AsyncClient = lambda *a, **kw: original_client(*a, transport=transport, **kw)
    try:
        graphs = {"my_agent": _build_graph()}
        await report_agent_schemas(graphs, "http://fake-control-plane:2026", "test-token", "python-langgraph")
    finally:
        si_module.httpx.AsyncClient = original_client

    check("exactly one PUT request was made", len(captured) == 1)
    req = captured[0]
    check("request method is PUT", req["method"] == "PUT")
    check(
        "URL targets the right agent_id", req["url"] == "http://fake-control-plane:2026/internal/agents/my_agent/schema"
    )
    check("runner-kind header set", req["headers"].get("x-runner-kind") == "python-langgraph")
    check("runner-token header set", req["headers"].get("x-runner-token") == "test-token")
    check("body carries the real schema, not the stub", "messages" in req["body"]["input_schema"].get("properties", {}))


async def test_report_agent_schemas_skips_failures_without_raising():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(500, json={"message": "internal error"})

    transport = httpx.MockTransport(handler)
    import runkite_runner.schema_introspect as si_module

    original_client = httpx.AsyncClient
    si_module.httpx.AsyncClient = lambda *a, **kw: original_client(*a, transport=transport, **kw)
    try:
        graphs = {"my_agent": _build_graph()}
        # Must not raise even though every report fails server-side.
        await report_agent_schemas(graphs, "http://fake-control-plane:2026", "", "python-langgraph")
        check("report_agent_schemas does not raise when a report fails", True)
    finally:
        si_module.httpx.AsyncClient = original_client


async def test_report_agent_schemas_noop_for_empty_graphs():
    # Must not construct an httpx client (or do anything) for zero graphs.
    await report_agent_schemas({}, "http://fake-control-plane:2026", "", "python-langgraph")
    check("report_agent_schemas is a no-op for an empty graphs dict", True)


def main():
    test_extract_agent_schema_pulls_real_field_names()
    test_extract_agent_schema_field_failure_falls_back_independently()
    asyncio.run(test_report_agent_schemas_puts_real_schema_with_auth_headers())
    asyncio.run(test_report_agent_schemas_skips_failures_without_raising())
    asyncio.run(test_report_agent_schemas_noop_for_empty_graphs())
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
