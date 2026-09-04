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

Input/output: with opaque checkpoints, prior thread messages are restored
into the agent's model_context before `run(task=new_text)` so clients can
send only the new turn. Without checkpoints, context is cleared each run
(no cross-thread leak) and only the last human text is forwarded as task.

Concurrency: an `AssistantAgent` keeps conversation history in
`self._model_context`. Concurrent `run()` calls would interleave appends,
so a per-graph_id lock serializes them (correctness over parallelism).
"""

from __future__ import annotations

import asyncio
import importlib.util
import json
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any

from runkite_runner.adapter_checkpoint import (
    decode_messages_checkpoint,
    encode_messages_checkpoint,
    last_human_text,
    merge_messages_input,
    messages_from_values_event,
)
from runkite_runner.generic_worker import EventCallback, RunCancelled, make_event_factory, run_cancellable
from runkite_runner.usage import usage_from_metrics, usage_or_unmetered, values_with_usage


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


def _usage_from_autogen_result(result: Any) -> dict[str, Any] | None:
    """Sum RequestUsage on each TaskResult message into Output.usage."""
    from runkite_runner.usage import usage_payload

    totals: dict[str, Any] = {}
    for msg in getattr(result, "messages", None) or []:
        mu = getattr(msg, "models_usage", None)
        if mu is None:
            continue
        p = int(getattr(mu, "prompt_tokens", 0) or 0)
        c = int(getattr(mu, "completion_tokens", 0) or 0)
        if p or c:
            totals["prompt_tokens"] = int(totals.get("prompt_tokens") or 0) + p
            totals["completion_tokens"] = int(totals.get("completion_tokens") or 0) + c
    return usage_payload(totals)


async def _seed_model_context(agent: Any, prior_messages: list) -> None:
    """Clear then replay prior turns into AutoGen's ChatCompletionContext.

    Must use ``autogen_core.models`` UserMessage/AssistantMessage — not
    ``autogen_agentchat.messages.TextMessage``. The OpenAI-compatible model
    client only transforms core LLMMessage types; seeding TextMessage makes
    the second turn on a checkpointed thread raise
    ``ValueError: Unknown message type: <class '...TextMessage'>``.
    """
    model_context = getattr(agent, "_model_context", None) or getattr(agent, "model_context", None)
    if model_context is None:
        return
    if hasattr(model_context, "clear"):
        await model_context.clear()
    if not prior_messages or not hasattr(model_context, "add_message"):
        return
    try:
        from autogen_core.models import AssistantMessage, UserMessage
    except ImportError:
        return
    for msg in prior_messages:
        if not isinstance(msg, dict):
            continue
        role = msg.get("role") or msg.get("type") or "user"
        content = msg.get("content") or ""
        if isinstance(content, list):
            content = " ".join(b.get("text", "") for b in content if isinstance(b, dict) and b.get("type") == "text")
        try:
            if role in ("human", "user"):
                await model_context.add_message(UserMessage(content=str(content), source="user"))
            else:
                await model_context.add_message(AssistantMessage(content=str(content), source="assistant"))
        except Exception:
            # Best-effort seed; run still proceeds with task= last human text.
            break


class AutoGenAdapter:
    """Loads AutoGen AssistantAgents from a config file and executes
    them against the Runner Protocol. See module docstring for
    conventions."""

    checkpoint_framework = "autogen"

    def __init__(self) -> None:
        self.agents: dict[str, Any] = {}
        # Serializes concurrent run() calls per graph_id -- see this
        # module's docstring for why a shared AssistantAgent's mutable
        # conversation context isn't safe to invoke concurrently.
        # defaultdict so concurrent first-uses of a graph_id can't race
        # to create two different Lock objects for it.
        self._agent_locks: dict[str, asyncio.Lock] = defaultdict(asyncio.Lock)

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
            await event_callback(
                make_event("error", {"message": f"unknown graph_id: {graph_id!r}. Available: {list(self.agents)}"})
            )
            return "error"

        await event_callback(make_event("lifecycle", {"event": "running"}))
        try:
            messages = list(input_data.get("messages", []))
            text = last_human_text(messages)
            prior = messages[:-1] if len(messages) > 1 else []

            # Lock (no concurrent run() on one agent) + seed or clear context.
            async with self._agent_locks[graph_id]:
                await _seed_model_context(agent, prior)
                result = await run_cancellable(agent.run(task=text), cancel_event)
            reply = _extract_text(result)

            output_messages = messages + [{"role": "ai", "content": reply}]
            # TaskResult.messages carry models_usage=RequestUsage(...); a
            # top-level usage_from_metrics(result) sees none of that, so FinOps
            # silently got $0 for every AutoGen Gemini run.
            usage = _usage_from_autogen_result(result) or usage_from_metrics(result)
            if usage and not usage.get("model"):
                # OpenAIChatCompletionClient keeps the id on _raw_config /
                # _resolved_model, not a public .model attribute.
                client = getattr(agent, "_model_client", None) or getattr(agent, "model_client", None)
                model = None
                if client is not None:
                    model = getattr(client, "_resolved_model", None) or getattr(client, "model", None)
                    if not model:
                        raw = getattr(client, "_raw_config", None) or {}
                        if isinstance(raw, dict):
                            model = raw.get("model")
                if model:
                    usage = {**usage, "model": str(model)}
            # A real reply came back but models_usage found nothing --
            # flag it explicitly instead of silently reporting zero spend.
            usage = usage_or_unmetered(usage, bool(reply and str(reply).strip()))
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
