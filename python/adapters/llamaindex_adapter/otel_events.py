"""LlamaIndex instrumentation handler that opens OTel child spans under
runkite.run.

Soft-optional: no-op when runner tracing is off or llama_index
instrumentation is unavailable. Metadata only (model/tool name).
"""

from __future__ import annotations

from collections.abc import Iterator
from contextlib import contextmanager
from contextvars import ContextVar
from typing import Any

# Per-async-task job state so concurrent --concurrency runs don't cross.
_job: ContextVar[dict[str, Any] | None] = ContextVar("runkite_li_otel_job", default=None)
_handler_attached = False


def _model_name(model_dict: Any) -> str:
    if isinstance(model_dict, dict):
        for key in ("model", "model_name", "model_id"):
            if model_dict.get(key):
                return str(model_dict[key])
    return ""


def _ensure_handler() -> bool:
    """Attach a process-wide handler once. Returns False if unavailable."""
    global _handler_attached
    if _handler_attached:
        return True
    try:
        from llama_index.core.instrumentation import get_dispatcher
        from llama_index.core.instrumentation.event_handlers import BaseEventHandler
        from llama_index.core.instrumentation.events.agent import AgentToolCallEvent
        from llama_index.core.instrumentation.events.llm import (
            LLMChatEndEvent,
            LLMChatStartEvent,
            LLMCompletionEndEvent,
            LLMCompletionStartEvent,
        )
        from opentelemetry.trace import Status, StatusCode
    except ImportError:
        return False

    class _Handler(BaseEventHandler):
        @classmethod
        def class_name(cls) -> str:
            return "RunkiteOTelEventHandler"

        def handle(self, event: Any, **kwargs: Any) -> Any:
            job = _job.get()
            if job is None:
                return None
            tracer = job["tracer"]
            parent_ctx = job["parent_ctx"]
            spans: dict[str, Any] = job["spans"]
            run_id = job["run_id"]

            if isinstance(event, (LLMChatStartEvent, LLMCompletionStartEvent)):
                key = getattr(event, "span_id", None) or getattr(event, "id_", None)
                if not key or key in spans:
                    return None
                span = tracer.start_span("runkite.llm", context=parent_ctx)
                name = _model_name(getattr(event, "model_dict", None))
                if name:
                    span.set_attribute("llm.name", name)
                if run_id:
                    span.set_attribute("run.id", run_id)
                spans[str(key)] = span
                return None

            if isinstance(event, (LLMChatEndEvent, LLMCompletionEndEvent)):
                key = getattr(event, "span_id", None) or getattr(event, "id_", None)
                span = spans.pop(str(key), None) if key else None
                if span is not None:
                    span.set_status(Status(StatusCode.OK))
                    span.end()
                return None

            if isinstance(event, AgentToolCallEvent):
                # Point-in-time event (no end) -- open and immediately close.
                tool = getattr(event, "tool", None)
                tool_name = getattr(tool, "metadata", None)
                if tool_name is not None and hasattr(tool_name, "name"):
                    name = str(tool_name.name)
                else:
                    name = str(getattr(tool, "name", "") or tool or "tool")
                span = tracer.start_span("runkite.tool", context=parent_ctx)
                if name:
                    span.set_attribute("tool.name", name)
                if run_id:
                    span.set_attribute("run.id", run_id)
                span.set_status(Status(StatusCode.OK))
                span.end()
            return None

    get_dispatcher().add_event_handler(_Handler())
    _handler_attached = True
    return True


@contextmanager
def attach_otel_job(run_id: str = "") -> Iterator[None]:
    """Bind this execute() call's parent context for the LI event handler."""
    try:
        from runkite_runner.tracing import get_tracer
    except ImportError:
        yield
        return

    tracer = get_tracer()
    if tracer is None or not _ensure_handler():
        yield
        return

    try:
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
        job = _job.get()
        _job.reset(token)
        if job:
            for span in list(job["spans"].values()):
                try:
                    span.set_status(Status(StatusCode.ERROR, "run ended with open span"))
                    span.end()
                except Exception:
                    pass
