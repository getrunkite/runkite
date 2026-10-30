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

from typing import Any
from urllib.parse import quote

import httpx

from .tenant_ctx import runner_headers
from .tls_utils import httpx_tls_kwargs

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
        self._runner_kind = runner_kind
        self._runner_token = runner_token
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
        return {
            **self._headers,
            **runner_headers(self._runner_token, self._runner_kind or None),
        }

    def _url(self, thread_id: str, checkpoint_id: str = ADAPTER_CHECKPOINT_ID) -> str:
        return f"{self._base}/internal/checkpoints/{quote(thread_id, safe='')}/{quote(checkpoint_id, safe='')}"

    async def get(self, thread_id: str, *, checkpoint_id: str = ADAPTER_CHECKPOINT_ID) -> bytes | None:
        """Legacy raw-byte GET; does not require CAS or framework response headers."""
        response = await self._get_response(thread_id, checkpoint_id)
        return None if response is None else response.content

    async def _get_response(self, thread_id: str, checkpoint_id: str) -> httpx.Response | None:
        if not thread_id:
            return None
        if checkpoint_id == "latest":
            raise ValueError('checkpoint_id "latest" is reserved; use a concrete id')
        client = await self._http()
        response = await client.get(self._url(thread_id, checkpoint_id), headers=self._client_headers())
        if response.status_code == 404:
            return None
        response.raise_for_status()
        return response

    async def get_versioned(
        self, thread_id: str, *, checkpoint_id: str = ADAPTER_CHECKPOINT_ID
    ) -> tuple[bytes | None, str | None]:
        """Return (blob, strong ETag) or (None, None) when missing.

        Managed adapters require both the expected HTTP framework and a valid
        version before executing any work. Legacy callers can keep using get.
        """
        if not thread_id:
            raise ValueError("thread_id required to load a versioned checkpoint")
        response = await self._get_response(thread_id, checkpoint_id)
        if response is None:
            return None, None
        if response.headers.get(HEADER_FRAMEWORK) != self._framework:
            raise RuntimeError("checkpoint GET returned a different or missing framework")
        etag = response.headers.get("ETag")
        if not _strong_numeric_etag(etag):
            raise RuntimeError("checkpoint GET requires a strong positive numeric ETag")
        return response.content, etag

    async def put(self, thread_id: str, data: bytes, *, checkpoint_id: str = ADAPTER_CHECKPOINT_ID) -> None:
        """Unconditional PUT of adapter state (overwrites same checkpoint_id)."""
        await self.put_cas(thread_id, data, etag=None, checkpoint_id=checkpoint_id, create_only=False)

    async def put_cas(
        self,
        thread_id: str,
        data: bytes,
        *,
        etag: str | None,
        checkpoint_id: str = ADAPTER_CHECKPOINT_ID,
        create_only: bool = False,
    ) -> None:
        """PUT with If-Match / If-None-Match. create_only uses If-None-Match: *."""
        if not thread_id:
            raise ValueError("thread_id required to save opaque checkpoint")
        if checkpoint_id == "latest":
            raise ValueError('checkpoint_id "latest" is reserved')
        if create_only and etag is not None:
            raise ValueError("create_only cannot combine with an If-Match etag")
        if not create_only and etag is not None and not _strong_numeric_etag(etag):
            raise ValueError("If-Match requires a strong positive numeric ETag")
        client = await self._http()
        headers = {
            **self._client_headers(),
            "Content-Type": "application/octet-stream",
        }
        if create_only:
            headers["If-None-Match"] = "*"
        elif etag is not None:
            headers["If-Match"] = etag
        resp = await client.put(self._url(thread_id, checkpoint_id), content=data, headers=headers)
        resp.raise_for_status()


def _strong_numeric_etag(etag: str | None) -> bool:
    """Only canonical positive int64 versions emitted by the Go plane are valid."""
    if not isinstance(etag, str) or len(etag) < 3 or etag[0] != '"' or etag[-1] != '"':
        return False
    value = etag[1:-1]
    return (
        1 <= len(value) <= 19
        and value[0] != "0"
        and value.isascii()
        and value.isdigit()
        and int(value) <= (1 << 63) - 1
    )


def adapter_supports_checkpoints(adapter: Any) -> bool:
    """True when adapter opts into opaque load/save via checkpoint_framework."""
    name = getattr(adapter, "checkpoint_framework", None)
    return isinstance(name, str) and bool(name.strip())
