"""Real JSON Schema introspection for a compiled LangGraph graph, reported
to the control plane (PUT /internal/agents/{agentID}/schema -- see
internal/api/agents.go's handleReportAgentSchema) to replace the
{"type":"object"} stub bootstrapAgents (cmd/serve.go) writes for every
agent at registration time. The control plane never loads a runner's
graph itself, so it has no way to know the real input/output/state/config
shape up front -- only the runner that actually loaded it does, which is
why this lives here and not on the Go side.

Only meaningful for a STATIC compiled graph (LangGraphAdapter.graphs, not
.factories) -- a factory graph's shape depends on per-run config/runtime
context that doesn't exist yet at load time, so introspecting it ahead of
time isn't meaningful the same way; factory graph_ids simply keep the
bootstrap stub.

Each of the four schema fields is extracted independently and falls back
to a plain {"type": "object"} on its own if that ONE extraction fails --
a compiled graph missing one exotic capability (e.g. no context_schema
declared) must not block reporting the other three, which are almost
always extractable.
"""

import logging
from typing import Any

import httpx

from .tls_utils import httpx_tls_kwargs

logger = logging.getLogger("runkite.runner")


def _safe(label: str, fn) -> dict[str, Any]:
    try:
        result = fn()
        if result is None:
            return {"type": "object"}
        return result
    except Exception as e:  # noqa: BLE001 -- deliberately broad: any single
        # extraction failing must not block the other three, or reporting
        # a real (if partial) schema instead of the stub at all.
        logger.debug(f"schema introspection: {label} extraction failed, falling back to stub: {e}")
        return {"type": "object"}


def extract_agent_schema(graph: Any) -> dict[str, dict[str, Any]]:
    """Returns {"input_schema", "output_schema", "state_schema",
    "config_schema"} for a compiled LangGraph StateGraph, using its own
    public introspection methods (get_input_jsonschema/
    get_output_jsonschema/get_config_jsonschema) plus its builder's
    state_schema (no dedicated get_state_jsonschema method exists on
    CompiledStateGraph as of LangGraph v1) converted via pydantic's
    TypeAdapter, the same mechanism LangGraph itself uses internally for
    the other three.
    """
    input_schema = _safe("input", graph.get_input_jsonschema)
    output_schema = _safe("output", graph.get_output_jsonschema)

    def _config():
        # get_config_jsonschema is deprecated in favor of
        # get_context_jsonschema (LangGraph v1+), but AgentSchema's own
        # "config_schema" field name matches the OLDER "config" concept
        # (the `configurable` sub-object) get_config_jsonschema still
        # returns, not the newer, distinct context_schema beta feature
        # Factory Graphs already handle separately (see factory_graph.py).
        # Deprecated != removed -- still the right method to call today.
        return graph.get_config_jsonschema()

    config_schema = _safe("config", _config)

    def _state():
        from pydantic import TypeAdapter

        return TypeAdapter(graph.builder.state_schema).json_schema()

    state_schema = _safe("state", _state)

    return {
        "input_schema": input_schema,
        "output_schema": output_schema,
        "state_schema": state_schema,
        "config_schema": config_schema,
    }


async def report_agent_schemas(graphs: dict[str, Any], http_base_url: str, runner_token: str, runner_kind: str) -> None:
    """Introspects and reports every STATIC graph's real schema to the
    control plane, once, at worker startup -- graphs is
    LangGraphAdapter.graphs (never .factories; see this module's own doc
    comment for why factory graphs are skipped). Best-effort per graph:
    one graph's report failing (network hiccup, control plane briefly
    unreachable) is logged and skipped, not fatal to startup -- the
    bootstrap stub simply stays in place for that one agent until the
    next restart tries again, same "degrade, don't crash" posture as
    every other optional-enhancement call this runner makes.
    """
    if not graphs:
        return
    headers: dict[str, str] = {}
    if runner_token:
        headers["X-Runner-Kind"] = runner_kind
        headers["X-Runner-Token"] = runner_token

    base_url = http_base_url.rstrip("/")
    async with httpx.AsyncClient(timeout=10.0, **httpx_tls_kwargs()) as client:
        for graph_id, graph in graphs.items():
            try:
                schema = extract_agent_schema(graph)
                resp = await client.put(f"{base_url}/internal/agents/{graph_id}/schema", json=schema, headers=headers)
                resp.raise_for_status()
                logger.info(f"Reported real schema for agent: {graph_id}")
            except Exception as e:  # noqa: BLE001 -- one graph's failure must not stop the others or fail startup
                logger.warning(f"Failed to report schema for agent {graph_id}, bootstrap stub stays in place: {e}")
