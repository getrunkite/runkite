"""LangChain callback handlers that open OTel child spans for LLM and
tool calls under the active runkite.run span.

Soft-imports langchain_core so adapters that never touch LangChain
(CrewAI, etc.) do not hard-fail on import. Prompt/completion bodies are
intentionally not recorded -- metadata only (model/tool name, status).
"""

from __future__ import annotations

from typing import Any
from uuid import UUID

try:
    from langchain_core.callbacks import BaseCallbackHandler
except ImportError:  # pragma: no cover - exercised when LC absent
    BaseCallbackHandler = object  # type: ignore[misc,assignment]


def new_handler(tracer: Any, parent_context: Any, run_id: str = "") -> Any | None:
    """Build an OTelCallbackHandler, or None if langchain_core is missing."""
    if BaseCallbackHandler is object:
        return None
    return OTelCallbackHandler(tracer, parent_context, run_id=run_id)


def _model_name(serialized: dict | None, metadata: dict | None) -> str:
    meta = metadata or {}
    for key in ("ls_model_name", "model_name", "model"):
        if meta.get(key):
            return str(meta[key])
    if not serialized:
        return ""
    name = serialized.get("name")
    if name:
        return str(name)
    ids = serialized.get("id")
    if isinstance(ids, list) and ids:
        return str(ids[-1])
    return ""


def _tool_name(serialized: dict | None, metadata: dict | None) -> str:
    meta = metadata or {}
    if meta.get("name"):
        return str(meta["name"])
    if not serialized:
        return ""
    name = serialized.get("name")
    if name:
        return str(name)
    ids = serialized.get("id")
    if isinstance(ids, list) and ids:
        return str(ids[-1])
    return ""


class OTelCallbackHandler(BaseCallbackHandler):
    """Opens runkite.llm / runkite.tool spans nested under runkite.run.

    Parenting uses LangChain's parent_run_id when the parent LC run already
    has an open span (tool under LLM); otherwise the OTel context captured
    at construction (the active runkite.run) is used.
    """

    def __init__(self, tracer: Any, parent_context: Any, run_id: str = "") -> None:
        super().__init__()
        self._tracer = tracer
        self._parent_context = parent_context
        self._assignment_run_id = run_id
        self._spans: dict[UUID, Any] = {}

    def __deepcopy__(self, memo: dict) -> OTelCallbackHandler:
        # Spans in _spans are not deepcopy-able; LC sometimes copies handlers.
        return self

    def _start_child(
        self,
        name: str,
        *,
        run_id: UUID,
        parent_run_id: UUID | None,
        attributes: dict[str, str],
    ) -> None:
        from opentelemetry import trace

        parent_ctx = self._parent_context
        if parent_run_id is not None and parent_run_id in self._spans:
            parent_ctx = trace.set_span_in_context(self._spans[parent_run_id])
        span = self._tracer.start_span(name, context=parent_ctx)
        for k, v in attributes.items():
            if v:
                span.set_attribute(k, v)
        if self._assignment_run_id:
            span.set_attribute("run.id", self._assignment_run_id)
        self._spans[run_id] = span

    def _end(self, run_id: UUID, *, error: BaseException | None = None) -> None:
        from opentelemetry.trace import Status, StatusCode

        span = self._spans.pop(run_id, None)
        if span is None:
            return
        if error is not None:
            span.set_status(Status(StatusCode.ERROR, str(error)))
            span.record_exception(error)
        else:
            span.set_status(Status(StatusCode.OK))
        span.end()

    def on_chat_model_start(
        self,
        serialized: dict[str, Any],
        messages: list,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        **kwargs: Any,
    ) -> Any:
        self._start_child(
            "runkite.llm",
            run_id=run_id,
            parent_run_id=parent_run_id,
            attributes={"llm.name": _model_name(serialized, metadata)},
        )

    def on_llm_start(
        self,
        serialized: dict[str, Any],
        prompts: list[str],
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        **kwargs: Any,
    ) -> Any:
        self._start_child(
            "runkite.llm",
            run_id=run_id,
            parent_run_id=parent_run_id,
            attributes={"llm.name": _model_name(serialized, metadata)},
        )

    def on_llm_end(self, response: Any, *, run_id: UUID, **kwargs: Any) -> Any:
        self._end(run_id)

    def on_llm_error(self, error: BaseException, *, run_id: UUID, **kwargs: Any) -> Any:
        self._end(run_id, error=error)

    def on_tool_start(
        self,
        serialized: dict[str, Any],
        input_str: str,
        *,
        run_id: UUID,
        parent_run_id: UUID | None = None,
        tags: list[str] | None = None,
        metadata: dict[str, Any] | None = None,
        inputs: dict[str, Any] | None = None,
        **kwargs: Any,
    ) -> Any:
        self._start_child(
            "runkite.tool",
            run_id=run_id,
            parent_run_id=parent_run_id,
            attributes={"tool.name": _tool_name(serialized, metadata)},
        )

    def on_tool_end(self, output: Any, *, run_id: UUID, **kwargs: Any) -> Any:
        self._end(run_id)

    def on_tool_error(self, error: BaseException, *, run_id: UUID, **kwargs: Any) -> Any:
        self._end(run_id, error=error)
