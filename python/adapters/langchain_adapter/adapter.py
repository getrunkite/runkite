"""Plain-LangChain adapter -- proves the Runner Protocol works for a
LangChain `Runnable` (a prompt|llm|parser pipe, an `AgentExecutor`, any
`Runnable`-satisfying chain) with NONE of LangGraph's platform-hosting
machinery: no StateGraph, no LangGraph checkpointer, no Factory Graph
convention. Deliberately the simplest possible adapter -- see
runkite_runner.generic_worker.FrameworkAdapter for the two methods any
adapter must implement.

Config convention matches LangGraphAdapter's langgraph.json shape
(`{"graphs": {"my_chain": "./chain.py:chain"}}`), so an agent author
switching from LangGraph to a plain chain doesn't need to learn a new
config format -- only `runner_kind` in that file changes.

Input/output convention: extracts text from `RunAssignment.input.messages`
and invokes the Runnable with `{"input": <text>}`. With opaque checkpoints,
prior turns are restored and folded into that string so clients can send
only the new message.
"""

from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path
from typing import Any

from runkite_runner.adapter_checkpoint import (
    context_prompt_from_messages,
    decode_messages_checkpoint,
    encode_messages_checkpoint,
    merge_messages_input,
    messages_from_values_event,
)
from runkite_runner.generic_worker import EventCallback, RunCancelled, make_event_factory, run_cancellable
from runkite_runner.tracing import make_run_callbacks
from runkite_runner.usage import accumulate_usage, usage_from_metrics, usage_payload, values_with_usage


def _extract_text(result: Any) -> str:
    """A Runnable's output shape varies by how the chain ends: a plain
    string (StrOutputParser), a BaseMessage (chat model with no output
    parser), or something else entirely -- normalize to text either way."""
    if isinstance(result, str):
        return result
    if hasattr(result, "content"):
        return result.content
    return str(result)


class LangChainAdapter:
    """Loads plain LangChain Runnables from a config file and executes
    them against the Runner Protocol. See module docstring for the
    config/input/output conventions."""

    checkpoint_framework = "langchain"

    def __init__(self) -> None:
        self.runnables: dict[str, Any] = {}

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
            spec = importlib.util.spec_from_file_location(f"runkite_langchain.{graph_id}", str(abs_path))
            if spec is None or spec.loader is None:
                raise ValueError(f"Cannot load chain module: {abs_path}")
            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)

            export = getattr(module, export_name)
            # A 0-arg factory (e.g. building the chain needs a client
            # constructed at startup) is supported, matching the
            # LangGraph runner's same "0-arg callable, resolved once"
            # convention -- anything with .ainvoke is used as-is.
            runnable = export() if callable(export) and not hasattr(export, "ainvoke") else export
            self.runnables[graph_id] = runnable

    async def execute(self, assignment: dict, event_callback: EventCallback, cancel_event) -> str:
        run_id = assignment["run_id"]
        graph_id = assignment["graph_id"]
        input_data = assignment.get("input") or {}
        make_event = make_event_factory(run_id)

        runnable = self.runnables.get(graph_id)
        if runnable is None:
            await event_callback(
                make_event("error", {"message": f"unknown graph_id: {graph_id!r}. Available: {list(self.runnables)}"})
            )
            return "error"

        await event_callback(make_event("lifecycle", {"event": "running"}))
        try:
            messages = list(input_data.get("messages", []))
            text = context_prompt_from_messages(messages)

            # Only pass config when we have callbacks -- a bare ainvoke(input)
            # keeps working for minimal/test Runnables that don't take config.
            if otel_cbs := make_run_callbacks(run_id):
                invoke = runnable.ainvoke({"input": text}, config={"callbacks": otel_cbs})
            else:
                invoke = runnable.ainvoke({"input": text})
            result = await run_cancellable(invoke, cancel_event)
            reply = _extract_text(result)

            output_messages = messages + [{"role": "ai", "content": reply}]
            totals: dict = {}
            accumulate_usage(totals, {"messages": [result] if result is not None else []})
            usage = usage_payload(totals) or usage_from_metrics(result)
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
