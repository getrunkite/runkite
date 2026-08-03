"""CrewAI event-bus listeners that open OTel child spans under runkite.run.

Soft-optional: no-op when runner tracing is off or crewai events are
unavailable. Metadata only (model/tool name) -- no prompts/completions.
CrewAI's own product tracing stays disabled (CREWAI_TRACING_ENABLED=false).
"""

from __future__ import annotations

from collections.abc import Iterator
from contextlib import contextmanager
from typing import Any


@contextmanager
def attach_otel_listeners(run_id: str = "") -> Iterator[None]:
    """Register LLM/tool listeners for the duration of one execute() call."""
    try:
        from runkite_runner.tracing import get_tracer
    except ImportError:
        yield
        return

    tracer = get_tracer()
    if tracer is None:
        yield
        return

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
        from opentelemetry import context as otel_context
        from opentelemetry.trace import Status, StatusCode
    except ImportError:
        yield
        return

    parent_ctx = otel_context.get_current()
    spans: dict[str, Any] = {}
    pairs: list[tuple[type, Any]] = []

    def _start(name: str, keys: list[str], attrs: dict[str, str]) -> None:
        keys = [k for k in keys if k]
        if not keys or any(k in spans for k in keys):
            return
        span = tracer.start_span(name, context=parent_ctx)
        for k, v in attrs.items():
            if v:
                span.set_attribute(k, v)
        if run_id:
            span.set_attribute("run.id", run_id)
        for k in keys:
            spans[k] = span

    def _end(keys: list[str], error: str | None = None) -> None:
        span = None
        for k in keys:
            if k and k in spans:
                span = spans.pop(k)
                break
        if span is None:
            return
        # Drop every alias pointing at the same span.
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

    bindings = [
        (LLMCallStartedEvent, on_llm_start),
        (LLMCallCompletedEvent, on_llm_done),
        (LLMCallFailedEvent, on_llm_fail),
        (ToolUsageStartedEvent, on_tool_start),
        (ToolUsageFinishedEvent, on_tool_done),
        (ToolUsageErrorEvent, on_tool_err),
    ]
    for et, handler in bindings:
        crewai_event_bus.register_handler(et, handler)
        pairs.append((et, handler))

    try:
        yield
    finally:
        # emit() runs handlers on a thread pool / asyncio; wait so end
        # events close spans before we unregister and leave the job.
        try:
            crewai_event_bus.flush(timeout=5.0)
        except Exception:
            pass
        for et, handler in pairs:
            try:
                crewai_event_bus.off(et, handler)
            except Exception:
                pass
        for span in list(spans.values()):
            try:
                span.set_status(Status(StatusCode.ERROR, "run ended with open span"))
                span.end()
            except Exception:
                pass
        spans.clear()
