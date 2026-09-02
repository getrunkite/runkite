"""Thin HTTP client for opaque /internal/checkpoints (generic adapters).

LangGraph uses ProxyCheckpointSaver for its own blob format. Non-LangGraph
adapters store a simpler framework-owned JSON/bytes blob under a stable
checkpoint_id (ADAPTER_CHECKPOINT_ID) and load that same id on the next
run — not GET .../latest, which is ordered by checkpoint_id DESC and can
return a LangGraph UUID blob on a shared thread.

Requires run-binding headers (X-Runkite-Run-Id / Generation) plus tenant —
callers must bind_tenant/bind_run (ContextVar) before get/put, same as
the LangGraph proxy path.
"""

from __future__ import annotations

import logging
from typing import Any
from urllib.parse import quote

import httpx

from .tenant_ctx import tenant_headers
from .tls_utils import httpx_tls_kwargs

logger = logging.getLogger("runkite.checkpoint.opaque")

HEADER_FRAMEWORK = "X-Runkite-Checkpoint-Framework"
# Stable id per thread for adapter state (not "latest" — that path is reserved).
ADAPTER_CHECKPOINT_ID = "adapter-state"


class OpaqueCheckpointClient:
    """GET/PUT opaque bytes for one runner process (fixed adapter-state id)."""

    def __init__(
        self,
        *,
        http_base_url: str,
        framework: str,
        runner_kind: str = "",
        runner_token: str | None = None,
        transport: httpx.AsyncBaseTransport | None = None,
    ):
        self._base = http_base_url.rstrip("/")
        self._framework = framework
        self._headers: dict[str, str] = {
            HEADER_FRAMEWORK: framework,
        }
        if runner_kind:
            self._headers["X-Runner-Kind"] = runner_kind
        if runner_token:
            self._headers["Authorization"] = f"Bearer {runner_token}"
            self._headers["X-Runner-Token"] = runner_token
        self._transport = transport
        self._client: httpx.AsyncClient | None = None

    async def _http(self) -> httpx.AsyncClient:
        if self._client is None:
            kwargs: dict[str, Any] = {"timeout": 30.0, **httpx_tls_kwargs()}
            if self._transport is not None:
                kwargs["transport"] = self._transport
            self._client = httpx.AsyncClient(**kwargs)
        return self._client

    async def aclose(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    def _client_headers(self) -> dict[str, str]:
        return {**self._headers, **tenant_headers()}

    def _url(self, thread_id: str, checkpoint_id: str = ADAPTER_CHECKPOINT_ID) -> str:
        return f"{self._base}/internal/checkpoints/{quote(thread_id, safe='')}/{quote(checkpoint_id, safe='')}"

    async def get(self, thread_id: str, *, checkpoint_id: str = ADAPTER_CHECKPOINT_ID) -> bytes | None:
        """Return this adapter's opaque blob for thread, or None if missing."""
        if not thread_id:
            return None
        if checkpoint_id == "latest":
            raise ValueError('checkpoint_id "latest" is reserved; use a concrete id')
        client = await self._http()
        resp = await client.get(self._url(thread_id, checkpoint_id), headers=self._client_headers())
        if resp.status_code == 404:
            return None
        resp.raise_for_status()
        return resp.content

    async def put(self, thread_id: str, data: bytes, *, checkpoint_id: str = ADAPTER_CHECKPOINT_ID) -> None:
        """Unconditional PUT of adapter state (overwrites same checkpoint_id)."""
        if not thread_id:
            raise ValueError("thread_id required to save opaque checkpoint")
        if checkpoint_id == "latest":
            raise ValueError('checkpoint_id "latest" is reserved')
        client = await self._http()
        headers = {
            **self._client_headers(),
            "Content-Type": "application/octet-stream",
        }
        resp = await client.put(self._url(thread_id, checkpoint_id), content=data, headers=headers)
        if resp.status_code == 403 and "run_not_inflight" in resp.text:
            logger.warning(
                "opaque checkpoint put dropped (run not in-flight) thread_id=%s framework=%s",
                thread_id,
                self._framework,
            )
            return
        resp.raise_for_status()


def adapter_supports_checkpoints(adapter: Any) -> bool:
    """True when adapter opts into opaque load/save via checkpoint_framework."""
    name = getattr(adapter, "checkpoint_framework", None)
    return isinstance(name, str) and bool(name.strip())
