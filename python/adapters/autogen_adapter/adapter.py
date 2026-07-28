"""AutoGen adapter -- proves the Runner Protocol works for Microsoft's
AutoGen (the `autogen-agentchat` package), a fundamentally different
execution model from LangGraph's StateGraph: a single `AssistantAgent`
wrapping a `ChatCompletionClient`, invoked via `agent.run(task=...)`.
See runkite_runner.generic_worker.FrameworkAdapter for the two methods
any adapter must implement.

Config convention matches LangGraphAdapter's langgraph.json shape
(`{"graphs": {"my_agent": "./agent.py:agent"}}`) -- same reasoning as
every other adapter: switching frameworks changes `runner_kind`, not
the config format.

Input/output convention: same as every other adapter -- extracts the
last human/user message's text from `RunAssignment.input.messages` and
calls `agent.run(task=<text>)`, using AutoGen's own native async run
(no thread-pool wrapping needed).

Concurrency note (runner-side concurrency spot-check, same discipline
as the CrewAI adapter's): an `AssistantAgent` keeps its own
conversation history in `self._model_context` (a
`ChatCompletionContext`, e.g. `UnboundedChatCompletionContext`), a
single shared, mutable object appended to on every `run()` call. Two
concurrent `run()` calls on the SAME shared agent instance would
interleave appends to that one context, corrupting both runs' history
-- there's no per-invocation isolation, same shape as CrewAI's shared
`Crew.usage_metrics`/`_task_output_handler`. Since `load_config` builds
one `AssistantAgent` per graph_id and shares it across every run
dispatched to that graph_id (same convention as every other adapter's
static graphs), a per-graph_id lock below serializes concurrent runs
on the same agent -- correct, though it means AutoGen runs sharing a
graph_id don't get real parallelism from `--concurrency > 1` the way
LangGraph/LangChain/LlamaIndex runs do. Building a fresh AssistantAgent
per run (LangGraph's Factory Graph pattern) would remove this ceiling;
not done here since it's a bigger change than a concurrency-safety fix
warrants on its own.
"""

from __future__ import annotations

import asyncio
import importlib.util
import json
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any

from runkite_runner.generic_worker import EventCallback, RunCancelled, make_event_factory, run_cancellable


def _last_human_text(messages: list) -> str:
    for msg in reversed(messages):
        role = msg.get("role") or msg.get("type") if isinstance(msg, dict) else getattr(msg, "type", None)
        if role in ("human", "user"):
            content = msg.get("content") if isinstance(msg, dict) else getattr(msg, "content", None)
            if isinstance(content, str):
                return content
            if isinstance(content, list):
                parts = [b.get("text", "") for b in content if isinstance(b, dict) and b.get("type") == "text"]
                return " ".join(parts)
    return ""


def _extract_text(result: Any) -> str:
    """AssistantAgent.run() returns a TaskResult whose .messages is the
    full conversation (task message + agent reply); the reply is the
    last message's .content. Falls back to str() for anything a custom
    agent might return that doesn't match this shape."""
    messages = getattr(result, "messages", None)
    if messages:
        content = getattr(messages[-1], "content", None)
        if isinstance(content, str):
            return content
    return str(result)


class AutoGenAdapter:
    """Loads AutoGen AssistantAgents from a config file and executes
    them against the Runner Protocol. See module docstring for
    conventions."""

    def __init__(self) -> None:
        self.agents: dict[str, Any] = {}
        # Serializes concurrent run() calls per graph_id -- see this
        # module's docstring for why a shared AssistantAgent's mutable
        # conversation context isn't safe to invoke concurrently.
        # defaultdict so concurrent first-uses of a graph_id can't race
        # to create two different Lock objects for it.
        self._agent_locks: dict[str, asyncio.Lock] = defaultdict(asyncio.Lock)

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
            spec = importlib.util.spec_from_file_location(f"runkite_autogen.{graph_id}", str(abs_path))
            if spec is None or spec.loader is None:
                raise ValueError(f"Cannot load agent module: {abs_path}")
            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)

            export = getattr(module, export_name)
            # 0-arg factory support (building an AssistantAgent fresh per
            # process start, e.g. reading an API key at startup) -- same
            # convention as every other adapter.
            agent = export() if callable(export) and not hasattr(export, "run") else export
            self.agents[graph_id] = agent

    async def execute(self, assignment: dict, event_callback: EventCallback, cancel_event) -> str:
        run_id = assignment["run_id"]
        graph_id = assignment["graph_id"]
        input_data = assignment.get("input") or {}
        make_event = make_event_factory(run_id)

        agent = self.agents.get(graph_id)
        if agent is None:
            await event_callback(make_event("error", {"message": f"unknown graph_id: {graph_id!r}. Available: {list(self.agents)}"}))
            return "error"

        await event_callback(make_event("lifecycle", {"event": "running"}))
        try:
            messages = list(input_data.get("messages", []))
            text = _last_human_text(messages)

            # See module docstring: a shared AssistantAgent's run() isn't
            # safe to invoke concurrently with itself (mutable
            # conversation context), so concurrent runs on the same
            # graph_id queue here rather than racing on that context.
            async with self._agent_locks[graph_id]:
                result = await run_cancellable(agent.run(task=text), cancel_event)
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
