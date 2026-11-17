"""Cache real schemas from static LangGraph graphs and report them best effort.

Each cached graph receives its existing initial schema PUT once, within bounded
rounds. Later visits only POST to the atomic bootstrap-stub recovery endpoint,
which cannot replace a useful or partial report. A recovery 404 never triggers
another PUT, including on older control planes.
Factories are not introspected because their shape can depend on run context.
"""

import asyncio
import contextlib
import contextvars
import json
import logging
from dataclasses import dataclass
from typing import Any
from urllib.parse import quote

import httpx

from .tls_utils import httpx_tls_kwargs

logger = logging.getLogger("runkite.runner")

MAX_SCHEMA_BYTES = 512 * 1024
MAX_CACHE_BYTES = 4 * 1024 * 1024
MAX_CACHED_GRAPHS = 64


def _safe(label: str, fn) -> dict[str, Any]:
    try:
        result = fn()
        if not isinstance(result, dict):
            return {"type": "object"}
        return result
    except Exception:  # noqa: BLE001 -- each field can fall back independently
        # Introspection exceptions can contain schema or application secrets.
        logger.debug("Schema %s extraction failed; using an object stub", label)
        return {"type": "object"}


def extract_agent_schema(graph: Any) -> dict[str, dict[str, Any]]:
    """Extract four fields from one trusted, already loaded static graph.

    This is synchronous startup introspection, not a sandbox for graph code.
    Each field has an independent fallback. The historical config field uses
    get_config_jsonschema, rather than the distinct context schema API.
    """
    input_schema = _safe("input", lambda: graph.get_input_jsonschema())
    output_schema = _safe("output", lambda: graph.get_output_jsonschema())
    config_schema = _safe("config", lambda: graph.get_config_jsonschema())

    def _state():
        from pydantic import TypeAdapter

        return TypeAdapter(graph.builder.state_schema).json_schema()

    return {
        "input_schema": input_schema,
        "output_schema": output_schema,
        "state_schema": _safe("state", _state),
        "config_schema": config_schema,
    }


@dataclass(frozen=True)
class _CachedSchema:
    url: str
    body: bytes


def _schema_bytes(schema: dict[str, Any]) -> bytes | None:
    """Bound retained serialized bytes without keeping an oversized JSON copy."""
    chunks: list[bytes] = []
    size = 0
    for chunk in json.JSONEncoder(separators=(",", ":"), allow_nan=False).iterencode(schema):
        encoded = chunk.encode("utf-8")
        size += len(encoded)
        if size > MAX_SCHEMA_BYTES:
            return None
        chunks.append(encoded)
    return b"".join(chunks)


class SchemaPublisher:
    """Own immutable schema bytes and one cancellable recovery task/client.

    Limits cover cached reports and asynchronous HTTP work. They do not impose
    a deadline on trusted graph introspection at construction time. Graphs
    beyond the cache bounds are skipped with a fixed diagnostic.
    """

    def __init__(
        self,
        graphs: dict[str, Any],
        http_base_url: str,
        runner_token: str,
        runner_kind: str,
        *,
        interval: float = 60.0,
        request_timeout: float = 5.0,
        round_timeout: float = 20.0,
    ) -> None:
        if interval <= 0 or request_timeout <= 0 or round_timeout <= 0:
            raise ValueError("Schema publisher time limits must be positive")
        self._interval = interval
        self._request_timeout = request_timeout
        self._round_timeout = round_timeout
        # Schema registration has no per-run or named-tenant routing. Never use
        # tenant_headers()/runner_headers(), which inherit assignment context.
        self._headers = {"Content-Type": "application/json", "X-Runkite-Tenant-Id": "default"}
        if runner_token:
            self._headers.update({"X-Runner-Kind": runner_kind, "X-Runner-Token": runner_token})
        reports: list[_CachedSchema] = []
        retained_bytes = 0
        for index, (graph_id, graph) in enumerate(graphs.items()):
            if index >= MAX_CACHED_GRAPHS:
                logger.warning("Schema cache graph limit reached; additional static graphs are not reported")
                break
            try:
                body = _schema_bytes(extract_agent_schema(graph))
                if body is None or retained_bytes + len(body) > MAX_CACHE_BYTES:
                    logger.warning("Schema cache byte limit reached; a static graph is not reported")
                    continue
                agent_path = quote(graph_id, safe="")
                if agent_path in {".", ".."}:
                    agent_path = agent_path.replace(".", "%2E")
                reports.append(_CachedSchema(f"{http_base_url.rstrip('/')}/internal/agents/{agent_path}/schema", body))
                retained_bytes += len(body)
            except Exception:  # noqa: BLE001 -- optional reporting must not fail worker startup
                logger.warning("Schema cache creation failed; a static graph is not reported")
        self._reports = tuple(reports)
        self._cursor = 0
        self._initial_attempted = [False] * len(self._reports)
        self._task: asyncio.Task | None = None

    @property
    def has_reports(self) -> bool:
        return bool(self._reports)

    def _client(self) -> httpx.AsyncClient:
        return httpx.AsyncClient(timeout=self._request_timeout, **httpx_tls_kwargs())

    async def _send(self, client: httpx.AsyncClient, report: _CachedSchema, *, recovery: bool) -> None:
        method = "POST" if recovery else "PUT"
        url = report.url + "/recover" if recovery else report.url
        # Do not buffer/read response bodies, including error bodies. They can
        # contain application data and are irrelevant to best-effort recovery.
        async with client.stream(method, url, content=report.body, headers=self._headers) as response:
            if not 200 <= response.status_code < 300:
                logger.warning("Schema report was not accepted (HTTP %d)", response.status_code)

    async def _round(self, client: httpx.AsyncClient, *, recovery: bool) -> None:
        deadline = asyncio.get_running_loop().time() + self._round_timeout
        for _ in self._reports:
            remaining = deadline - asyncio.get_running_loop().time()
            if remaining <= 0:
                return
            index = self._cursor
            report = self._reports[index]
            # Advance before awaiting so a slow graph cannot starve its peers.
            self._cursor = (self._cursor + 1) % len(self._reports)
            initial = not self._initial_attempted[index]
            if not recovery and not initial:
                continue
            # A failed initial attempt is still an attempt. Neither a network
            # error nor a later recovery 404 can authorize another normal PUT.
            self._initial_attempted[index] = True
            try:
                await asyncio.wait_for(
                    self._send(client, report, recovery=not initial),
                    timeout=min(self._request_timeout, remaining),
                )
            except Exception:  # noqa: BLE001 -- includes bounded network/timeout failures, never cancellation
                logger.warning("Schema report failed; recovery will retry cached reports")

    async def report_startup(self) -> None:
        """One bounded initial PUT round; background work drains any backlog."""
        if not self.has_reports:
            return
        try:
            async with self._client() as client:
                await self._round(client, recovery=False)
        except Exception:  # noqa: BLE001 -- preserve best-effort startup on TLS/client initialization errors
            logger.warning("Schema startup reporting could not initialize its HTTP client")

    async def _recover(self) -> None:
        while True:
            try:
                async with self._client() as client:
                    while True:
                        await self._round(client, recovery=True)
                        await asyncio.sleep(self._interval)
            except Exception:  # noqa: BLE001 -- TLS/client initialization may fail temporarily
                logger.warning("Schema recovery HTTP client failed; recovery will retry")
                await asyncio.sleep(self._interval)

    def start(self) -> asyncio.Task | None:
        """Start immediate recovery in a fresh context, or do nothing if empty."""
        if self.has_reports and self._task is None:
            self._task = asyncio.create_task(
                self._recover(), name="runkite-schema-recovery", context=contextvars.Context()
            )
        return self._task

    async def stop(self) -> None:
        """Cancel and await the owned task, including HTTP client cleanup."""
        task, self._task = self._task, None
        if task is not None:
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task


async def report_agent_schemas(graphs: dict[str, Any], http_base_url: str, runner_token: str, runner_kind: str) -> None:
    """Compatibility helper for one startup PUT round; no background task."""
    await SchemaPublisher(graphs, http_base_url, runner_token, runner_kind).report_startup()
