"""Connector session + MCP proxy helpers for agent node code.

`POST /internal/connectors/{name}/session` and `.../mcp` are run-bound:
the control plane requires `X-Runkite-Run-Id` + `X-Runkite-Generation`
matching an in-flight assignment (see docs/trust-governance.md). Store
and vector clients inherit those headers via `tenant_headers()`; connector
calls from graph code do not, unless they go through these helpers.

Same shape as `call_agent`: pass the node's own RunnableConfig (or its
`configurable` sub-dict). `build_run_config` sets `configurable.run_id`
and `configurable.generation` for every job.
"""

from __future__ import annotations

import os
from typing import Any

import httpx

from .tenant_ctx import HEADER_GENERATION, HEADER_RUN_ID, tenant_headers
from .tls_utils import httpx_tls_kwargs


class ConnectorError(Exception):
    """Control plane rejected a connector session or MCP proxy call."""


def _configurable(config: dict | None) -> dict:
    if not config:
        return {}
    if "configurable" in config and isinstance(config["configurable"], dict):
        return config["configurable"]
    # Caller passed the configurable sub-dict itself (common in nodes).
    return config


def _run_bound_headers(config: dict | None) -> dict[str, str]:
    cfg = _configurable(config)
    run_id = cfg.get("run_id")
    if not run_id:
        raise ValueError(
            "connector helper: config has no configurable.run_id -- must be "
            "called with the RunnableConfig a graph node itself received "
            "(build_run_config sets run_id and generation)"
        )
    headers = {**tenant_headers()}
    headers[HEADER_RUN_ID] = str(run_id)
    headers[HEADER_GENERATION] = str(int(cfg.get("generation") or 0))
    runner_token = os.environ.get("RUNNER_TOKEN")
    if runner_token:
        # Same hardcoded kind as store.py / a2a.py (no RUNNER_KIND env).
        headers["X-Runner-Kind"] = "python-langgraph"
        headers["X-Runner-Token"] = runner_token
    return headers


def _base_url(control_plane_url: str | None) -> str:
    return (control_plane_url or os.environ.get("RUNKITE_HTTP_URL") or "http://localhost:2026").rstrip("/")


async def get_connector_session(
    config: dict,
    name: str,
    *,
    user_context: dict[str, Any] | None = None,
    control_plane_url: str | None = None,
    timeout: float = 30.0,
) -> dict[str, Any]:
    """Mint a pre-authenticated connector session for this run.

    Args:
        config: the RunnableConfig LangGraph passes to the node (or its
            `configurable` sub-dict). Must carry `run_id` / `generation`.
        name: connector name from connectors/*.yaml.
        user_context: optional identity forwarded to the connector auth
            exchange (token-exchange flows).
        control_plane_url: defaults to RUNKITE_HTTP_URL.
        timeout: httpx timeout seconds.
    """
    headers = _run_bound_headers(config)
    body: dict[str, Any] = {}
    if user_context is not None:
        body["user_context"] = user_context
    url = f"{_base_url(control_plane_url)}/internal/connectors/{name}/session"
    async with httpx.AsyncClient(timeout=timeout, **httpx_tls_kwargs()) as client:
        resp = await client.post(url, json=body, headers=headers)
    if resp.status_code >= 400:
        raise ConnectorError(f"get_connector_session {name}: HTTP {resp.status_code}: {resp.text}")
    return resp.json()


async def proxy_connector_mcp(
    config: dict,
    name: str,
    request: dict[str, Any],
    *,
    control_plane_url: str | None = None,
    timeout: float = 60.0,
) -> dict[str, Any]:
    """Proxy one JSON-RPC request through the connector MCP gate.

    The control plane enforces tools.allow/deny before forwarding.
    """
    headers = _run_bound_headers(config)
    url = f"{_base_url(control_plane_url)}/internal/connectors/{name}/mcp"
    async with httpx.AsyncClient(timeout=timeout, **httpx_tls_kwargs()) as client:
        resp = await client.post(url, json=request, headers=headers)
    if resp.status_code >= 400:
        raise ConnectorError(f"proxy_connector_mcp {name}: HTTP {resp.status_code}: {resp.text}")
    return resp.json()
