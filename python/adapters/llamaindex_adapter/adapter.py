"""LlamaIndex adapter -- proves the Runner Protocol works for
LlamaIndex's own execution model (a ChatEngine/agent built around an
LLM + optional tools/retrievers), a different shape again from both
LangGraph's StateGraph and CrewAI's Agent/Task/Crew. See
runkite_runner.generic_worker.FrameworkAdapter for the two methods any
adapter must implement.

Config convention matches LangGraphAdapter's langgraph.json shape
(`{"graphs": {"my_engine": "./chat_engine.py:chat_engine"}}`) -- same
reasoning as langchain_adapter/crewai_adapter.

Input/output convention: extracts every message from
`RunAssignment.input.messages`. When opaque checkpoints are available
(RUNKITE_HTTP_URL / worker http address), generic_worker restores prior
thread messages so clients can send only the new turn. Without a
checkpoint, the client must still send full history each call (engines
are shared across runs and must not rely on in-process chat_history).
"""

from __future__ import annotations

import asyncio
import importlib.util
import json
import sys
from pathlib import Path
from typing import Any

from llama_index.core.base.llms.types import ChatMessage, MessageRole
from runkite_runner.adapter_checkpoint import (
    decode_messages_checkpoint,
    encode_messages_checkpoint,
    merge_messages_input,
    messages_from_values_event,
)
from runkite_runner.generic_worker import EventCallback, RunCancelled, make_event_factory, run_cancellable
from runkite_runner.usage import usage_from_metrics, usage_payload, values_with_usage

from .otel_events import attach_otel_job

# LLM.callback_manager is per-instance but shared across concurrent runs of
# the same graph_id; serialize install/uninstall so TokenCountingHandler
# counts don't mix between runs.
_settings_lock = asyncio.Lock()


def _to_chat_messages(messages: list) -> list[ChatMessage]:
    """Converts the Runner Protocol's {"role": ..., "content": ...}
    message dicts into LlamaIndex's own ChatMessage type."""
    role_map = {
        "human": MessageRole.USER,
        "user": MessageRole.USER,
        "ai": MessageRole.ASSISTANT,
        "assistant": MessageRole.ASSISTANT,
        "system": MessageRole.SYSTEM,
    }
    out = []
    for msg in messages:
        role = msg.get("role") or msg.get("type") if isinstance(msg, dict) else getattr(msg, "type", None)
        content = msg.get("content") if isinstance(msg, dict) else getattr(msg, "content", None)
        if isinstance(content, list):
            content = " ".join(b.get("text", "") for b in content if isinstance(b, dict) and b.get("type") == "text")
        out.append(ChatMessage(role=role_map.get(role, MessageRole.USER), content=content or ""))
    return out


def _extract_text(result: Any) -> str:
    """AgentChatResponse (from achat) exposes the final answer via
    .response; fall back to str() for anything else a custom engine
    might return."""
    if hasattr(result, "response"):
        return str(result.response)
    return str(result)


class LlamaIndexAdapter:
    """Loads LlamaIndex chat engines and executes them against the Runner Protocol."""

    # Opt into opaque multi-turn via generic_worker (GET/PUT adapter-state).
    checkpoint_framework = "llamaindex"

    def __init__(self) -> None:
        self.engines: dict[str, Any] = {}

    def prepare_checkpoint_input(self, prior: bytes | None, input_data: dict) -> dict:
        return merge_messages_input(decode_messages_checkpoint(prior), input_data)

    def serialize_checkpoint(self, assignment: dict, values: dict, status: str) -> bytes | None:
        msgs = messages_from_values_event(values)
        if not msgs:
            return None
        return encode_messages_checkpoint(msgs)

    async def load_config(self, config_path: str) -> None:
        path = Path(config_path)
        with open(path) as f:
            config = json.load(f)

        config_dir = path.parent
        for dep in config.get("dependencies", []):
            dep_path = (config_dir / dep).resolve()
            if str(dep_path) not in sys.path:
                sys.path.insert(0, str(dep_path))

        for graph_id, ref in config.get("graphs", {}).items():
            file_path, export_name = ref.split(":", 1)
            abs_path = (config_dir / file_path).resolve()
            spec = importlib.util.spec_from_file_location(f"runkite_llamaindex.{graph_id}", str(abs_path))
            if spec is None or spec.loader is None:
                raise ValueError(f"Cannot load chat engine module: {abs_path}")
            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)

            export = getattr(module, export_name)
            # 0-arg factory support, same convention as the other adapters.
            engine = export() if callable(export) and not hasattr(export, "achat") else export
            self.engines[graph_id] = engine

    async def execute(self, assignment: dict, event_callback: EventCallback, cancel_event) -> str:
        run_id = assignment["run_id"]
        graph_id = assignment["graph_id"]
        input_data = assignment.get("input") or {}
        make_event = make_event_factory(run_id)

        engine = self.engines.get(graph_id)
        if engine is None:
            await event_callback(
                make_event("error", {"message": f"unknown graph_id: {graph_id!r}. Available: {list(self.engines)}"})
            )
            return "error"

        await event_callback(make_event("lifecycle", {"event": "running"}))
        try:
            messages = list(input_data.get("messages", []))
            if not messages:
                raise ValueError("input.messages is empty -- nothing to respond to")

            last_text = (
                messages[-1].get("content") if isinstance(messages[-1], dict) else getattr(messages[-1], "content", "")
            )
            if isinstance(last_text, list):
                last_text = " ".join(
                    b.get("text", "") for b in last_text if isinstance(b, dict) and b.get("type") == "text"
                )
            prior_history = _to_chat_messages(messages[:-1])

            # GoogleGenAI + SimpleChatEngine returns AgentChatResponse with
            # empty metadata — usage_from_metrics sees nothing. Meter via
            # TokenCountingHandler on the *LLM instance* callback_manager
            # (Settings.callback_manager alone is ignored once the LLM was
            # constructed at adapter load time with a different manager).
            from llama_index.core.callbacks import CallbackManager, TokenCountingHandler

            llm = getattr(engine, "_llm", None) or getattr(engine, "llm", None)
            tok = TokenCountingHandler()
            async with _settings_lock:
                prev_cm = getattr(llm, "callback_manager", None) if llm is not None else None
                if llm is not None:
                    llm.callback_manager = CallbackManager([tok])
                try:
                    with attach_otel_job(run_id):
                        result = await run_cancellable(
                            engine.achat(last_text, chat_history=prior_history), cancel_event
                        )
                finally:
                    if llm is not None and prev_cm is not None:
                        llm.callback_manager = prev_cm
            reply = _extract_text(result)

            output_messages = messages + [{"role": "ai", "content": reply}]
            usage = None
            if tok.prompt_llm_token_count or tok.completion_llm_token_count:
                totals = {
                    "prompt_tokens": int(tok.prompt_llm_token_count or 0),
                    "completion_tokens": int(tok.completion_llm_token_count or 0),
                }
                model = getattr(llm, "model", None) or getattr(llm, "model_name", None) if llm else None
                if model:
                    totals["model"] = str(model)
                usage = usage_payload(totals)
            if usage is None:
                usage = usage_from_metrics(
                    getattr(result, "additional_kwargs", None)
                    or getattr(result, "raw", None)
                    or getattr(result, "meta", None)
                )
            if usage is None:
                usage = usage_from_metrics(getattr(result, "token_counts", None))
            values = values_with_usage({"messages": output_messages}, usage)
            await event_callback(make_event("values", values))
            await event_callback(make_event("end", {"status": "success"}))
            return "success"
        except RunCancelled:
            await event_callback(make_event("end", {"status": "interrupted"}))
            return "interrupted"
        except Exception as e:
            await event_callback(make_event("error", {"message": str(e), "type": type(e).__name__}))
            return "error"
