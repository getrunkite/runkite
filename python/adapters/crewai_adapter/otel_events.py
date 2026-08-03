"""CrewAI event-bus listeners that open OTel child spans under runkite.run.

Soft-optional: no-op when runner tracing is off or crewai events are
unavailable. Metadata only (model/tool name) -- no prompts/completions.
CrewAI's own product tracing stays disabled (CREWAI_TRACING_ENABLED=false).

crewai_event_bus is process-global: every registered handler sees every
emit. Register exactly one handler set and scope per-job state via a
ContextVar (CrewAI's sync emit path uses contextvars.copy_context(), so
concurrent --concurrency runs do not cross-attribute spans).
"""

from __future__ import annotations

from collections.abc import Iterator
from contextlib import contextmanager
from contextvars import ContextVar
from typing import Any

# Per-async-task / per-thread job state so concurrent --concurrency runs
# don't share span maps or parent contexts.
_job: ContextVar[dict[str, Any] | None] = ContextVar("runkite_crewai_otel_job", default=None)
_handlers_attached = False


def _ensure_handlers() -> bool:
    """Attach process-wide listeners once. Returns False if unavailable."""
    global _handlers_attached
    if _handlers_attached:
        return True
    try:
        from crewai.events import crewai_event_bus
        from crewai.events.types.llm_events import (
            LLMCallCompletedEvent,
            LLMCallFailedEvent,
            LLMCallStartedEvent,
        )
        from crewai.events.types.tool_usage_events import (
            ToolUsageErrorEvent,
            ToolUsageFinishedEvent,
            ToolUsageStartedEvent,
        )
        from opentelemetry.trace import Status, StatusCode
    except ImportError:
        return False

    def _start(name: str, keys: list[str], attrs: dict[str, str]) -> None:
        job = _job.get()
        if job is None:
            return
        spans: dict[str, Any] = job["spans"]
        keys = [k for k in keys if k]
        if not keys or any(k in spans for k in keys):
            return
        span = job["tracer"].start_span(name, context=job["parent_ctx"])
        for k, v in attrs.items():
            if v:
                span.set_attribute(k, v)
        run_id = job.get("run_id") or ""
        if run_id:
            span.set_attribute("run.id", run_id)
        for k in keys:
            spans[k] = span

    def _end(keys: list[str], error: str | None = None) -> None:
        job = _job.get()
        if job is None:
            return
        spans: dict[str, Any] = job["spans"]
        span = None
        for k in keys:
            if k and k in spans:
                span = spans.pop(k)
                break
        if span is None:
            return
        for k, s in list(spans.items()):
            if s is span:
                del spans[k]
        if error:
            span.set_status(Status(StatusCode.ERROR, error))
        else:
            span.set_status(Status(StatusCode.OK))
        span.end()

    def on_llm_start(_source: Any, event: Any) -> None:
        _start(
            "runkite.llm",
            [str(getattr(event, "call_id", "") or ""), str(getattr(event, "event_id", "") or "")],
            {"llm.name": str(getattr(event, "model", None) or "")},
        )

    def on_llm_done(_source: Any, event: Any) -> None:
        _end(
            [
                str(getattr(event, "call_id", "") or ""),
                str(getattr(event, "started_event_id", "") or ""),
                str(getattr(event, "event_id", "") or ""),
            ]
        )

    def on_llm_fail(_source: Any, event: Any) -> None:
        _end(
            [
                str(getattr(event, "call_id", "") or ""),
                str(getattr(event, "started_event_id", "") or ""),
                str(getattr(event, "event_id", "") or ""),
            ],
            error=str(getattr(event, "error", "llm failed")),
        )

    def on_tool_start(_source: Any, event: Any) -> None:
        _start(
            "runkite.tool",
            [str(getattr(event, "event_id", "") or "")],
            {"tool.name": str(getattr(event, "tool_name", "") or "")},
        )

    def on_tool_done(_source: Any, event: Any) -> None:
        _end(
            [
                str(getattr(event, "started_event_id", "") or ""),
                str(getattr(event, "event_id", "") or ""),
            ]
        )

    def on_tool_err(_source: Any, event: Any) -> None:
        _end(
            [
                str(getattr(event, "started_event_id", "") or ""),
                str(getattr(event, "event_id", "") or ""),
            ],
            error=str(getattr(event, "error", "tool failed")),
        )

    for et, handler in (
        (LLMCallStartedEvent, on_llm_start),
        (LLMCallCompletedEvent, on_llm_done),
        (LLMCallFailedEvent, on_llm_fail),
        (ToolUsageStartedEvent, on_tool_start),
        (ToolUsageFinishedEvent, on_tool_done),
        (ToolUsageErrorEvent, on_tool_err),
    ):
        crewai_event_bus.register_handler(et, handler)

    _handlers_attached = True
    return True


@contextmanager
def attach_otel_listeners(run_id: str = "") -> Iterator[None]:
    """Bind this execute() call's parent context for the shared listeners."""
    try:
        from runkite_runner.tracing import get_tracer
    except ImportError:
        yield
        return

    tracer = get_tracer()
    if tracer is None or not _ensure_handlers():
        yield
        return

    try:
        from crewai.events import crewai_event_bus
        from opentelemetry import context as otel_context
        from opentelemetry.trace import Status, StatusCode
    except ImportError:
        yield
        return

    token = _job.set(
        {
            "tracer": tracer,
            "parent_ctx": otel_context.get_current(),
            "spans": {},
            "run_id": run_id,
        }
    )
    try:
        yield
    finally:
        try:
            crewai_event_bus.flush(timeout=5.0)
        except Exception:
            pass
        job = _job.get()
        _job.reset(token)
        if job:
            for span in list(job["spans"].values()):
                try:
                    span.set_status(Status(StatusCode.ERROR, "run ended with open span"))
                    span.end()
                except Exception:
                    pass
