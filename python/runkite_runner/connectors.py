"""Run-bound connector session and MCP proxy helpers.

One total operation deadline covers session minting, capability renewal and
response bodies. Caller task cancellation closes the active HTTP client.
"""

from __future__ import annotations

import asyncio
import json
import math
import os
from typing import Any
from urllib.parse import quote

import httpx

from .tenant_ctx import HEADER_GENERATION, HEADER_RUN_ID, runner_headers
from .tls_utils import httpx_tls_kwargs

HEADER_CONNECTOR_SESSION = "X-Runkite-Connector-Session"


class ConnectorError(Exception):
    """Control plane rejected a connector session or MCP proxy call."""


def _configurable(config: dict | None) -> dict:
    if not config:
        return {}
    if "configurable" in config and isinstance(config["configurable"], dict):
        return config["configurable"]
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
    headers = runner_headers()
    headers[HEADER_RUN_ID] = str(run_id)
    headers[HEADER_GENERATION] = str(int(cfg.get("generation") or 0))
    return headers


def _base_url(control_plane_url: str | None) -> str:
    return (control_plane_url or os.environ.get("RUNKITE_HTTP_URL") or "http://localhost:2026").rstrip("/")


def _connector_name(name: str) -> str:
    if not isinstance(name, str) or not name or name in {".", ".."}:
        raise ValueError("connector name must be a nonempty path segment")
    return quote(name, safe="")


def _validate_timeout(timeout: float) -> None:
    valid = not isinstance(timeout, bool) and isinstance(timeout, (int, float))
    try:
        valid = valid and math.isfinite(timeout) and timeout > 0
    except OverflowError:
        valid = False
    if not valid:
        raise ValueError("connector timeout must be a positive finite number")


async def _session(client: httpx.AsyncClient, url: str, headers: dict[str, str], body: bytes) -> dict[str, Any]:
    resp = await client.post(url, content=body, headers={**headers, "Content-Type": "application/json"})
    if not resp.is_success:
        raise ConnectorError(f"get_connector_session: HTTP {resp.status_code}: {resp.text}")
    return resp.json()


async def get_connector_session(
    config: dict,
    name: str,
    *,
    user_context: dict[str, Any] | None = None,
    control_plane_url: str | None = None,
    timeout: float = 30.0,
) -> dict[str, Any]:
    """Mint a session, bounded by one total deadline (default 30 seconds)."""
    _validate_timeout(timeout)
    headers = _run_bound_headers(config)
    url = f"{_base_url(control_plane_url)}/internal/connectors/{_connector_name(name)}/session"
    body = json.dumps(
        {} if user_context is None else {"user_context": user_context},
        allow_nan=False,
        ensure_ascii=False,
        separators=(",", ":"),
    ).encode("utf-8")
    async with asyncio.timeout(timeout), httpx.AsyncClient(timeout=timeout, **httpx_tls_kwargs()) as client:
        return await _session(client, url, headers, body)


async def proxy_connector_mcp(
    config: dict,
    name: str,
    request: dict[str, Any],
    *,
    session_token: str | None = None,
    control_plane_url: str | None = None,
    timeout: float = 60.0,
) -> dict[str, Any]:
    """Proxy one JSON-RPC request within one total deadline (default 60 seconds).

    Only the plane's explicit pre-dispatch session rejection permits one automatic
    remint/retry. Caller-supplied session tokens and arbitrary 401s never retry.
    """
    _validate_timeout(timeout)
    headers = _run_bound_headers(config)
    url = f"{_base_url(control_plane_url)}/internal/connectors/{_connector_name(name)}"
    body = json.dumps(request, allow_nan=False, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    async with asyncio.timeout(timeout), httpx.AsyncClient(timeout=timeout, **httpx_tls_kwargs()) as client:

        async def mint() -> str:
            value = await _session(client, f"{url}/session", headers, b"{}")
            token = value.get("session_token")
            if not isinstance(token, str) or not token:
                raise ConnectorError(f"proxy_connector_mcp {name}: session response missing session_token")
            return token

        async def once(token: str) -> httpx.Response:
            return await client.post(
                f"{url}/mcp",
                content=body,
                headers={**headers, HEADER_CONNECTOR_SESSION: token, "Content-Type": "application/json"},
            )

        resp = await once(session_token or await mint())
        if resp.status_code == 401 and session_token is None:
            # Upstream 401 can follow a real side effect. Retry only a capability
            # rejection that the control plane identifies as pre-dispatch.
            try:
                error = resp.json()
            except ValueError:
                error = None
            if isinstance(error, dict) and error.get("reason_code") == "connector_session_invalid":
                resp = await once(await mint())
        if not resp.is_success:
            raise ConnectorError(f"proxy_connector_mcp {name}: HTTP {resp.status_code}: {resp.text}")
        return resp.json()
