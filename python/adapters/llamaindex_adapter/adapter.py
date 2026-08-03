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
`RunAssignment.input.messages` (not just the last one, unlike
langchain_adapter/crewai_adapter) because a LlamaIndex ChatEngine object
is typically SHARED across every run dispatched to this runner process,
so its own internal `.chat_history` must never be relied on for
per-thread state -- doing so would leak one thread's conversation into
another's. Instead, this adapter reconstructs the full prior history
from RunAssignment.input every call and passes it explicitly via
`achat(message, chat_history=...)`, which every real chat-engine/agent
class in LlamaIndex accepts for exactly this reason.
"""

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path
from typing import Any

from llama_index.core.base.llms.types import ChatMessage, MessageRole
from runkite_runner.generic_worker import EventCallback, RunCancelled, make_event_factory, run_cancellable

from .otel_events import attach_otel_job


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
        return result.response
    if isinstance(result, str):
        return result
    return str(result)


class LlamaIndexAdapter:
    """Loads LlamaIndex chat engines/agents from a config file and
    executes them against the Runner Protocol. See module docstring for
    conventions, especially the per-call chat_history reconstruction."""

    def __init__(self) -> None:
        self.engines: dict[str, Any] = {}

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
            prior_history = _to_chat_messages(messages[:-1])

            # Nest runkite.llm/tool under the active runkite.run (no-op when OTEL is off).
            with attach_otel_job(run_id):
                result = await run_cancellable(engine.achat(last_text, chat_history=prior_history), cancel_event)
            reply = _extract_text(result)

            output_messages = messages + [{"role": "ai", "content": reply}]
            await event_callback(make_event("values", {"messages": output_messages}))
            await event_callback(make_event("end", {"status": "success"}))
            return "success"
        except RunCancelled:
            await event_callback(make_event("end", {"status": "interrupted"}))
            return "interrupted"
        except Exception as e:
            await event_callback(make_event("error", {"message": str(e), "type": type(e).__name__}))
            return "error"
