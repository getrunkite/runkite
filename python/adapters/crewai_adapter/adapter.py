"""CrewAI adapter -- proves the Runner Protocol works for CrewAI's own
execution model (Agents + Tasks + Crew.kickoff), a fundamentally
different shape from LangGraph's StateGraph. See
runkite_runner.generic_worker.FrameworkAdapter for the two methods any
adapter must implement.

Config convention matches LangGraphAdapter's langgraph.json shape
(`{"graphs": {"my_crew": "./crew.py:crew"}}`) -- same reasoning as
langchain_adapter: an agent author switching frameworks doesn't need a
new config format, only `runner_kind` changes.

Input/output convention: extracts human/user text from
`RunAssignment.input.messages` and calls `crew.akickoff(inputs={"input":
<text>})`. With opaque checkpoints enabled, prior turns are restored and
folded into the input string so clients can send only the new message
(CrewAI itself has no LangGraph-style checkpointer; this is plane-owned
message continuity, not full crew-memory restore).

Concurrency note (runner-side concurrency spot-check): unlike a
LangGraph compiled graph or a plain LangChain Runnable, a CrewAI `Crew`
instance is NOT safe to invoke concurrently -- confirmed by reading
crewai's own Crew.kickoff/akickoff (crewai/crew.py): both write results
directly onto shared instance attributes (`self.usage_metrics`,
`self._task_output_handler.reset()`/`.update()`), which would race and
corrupt each other if two akickoff() calls ran on the SAME shared Crew
object at once -- there's no per-invocation isolation the way a
LangGraph graph's checkpointer-keyed-by-thread_id gives it. Since
load_config builds one Crew per graph_id and shares it across every run
dispatched to that graph_id (same convention as LangGraphAdapter's
static graphs), a per-graph_id lock below serializes concurrent
akickoff() calls on the same Crew -- correct, though it means CrewAI
runs sharing a graph_id don't get real parallelism from
--concurrency > 1 the way LangGraph/LangChain/LlamaIndex runs do.
Building a fresh Crew per run (LangGraph's Factory Graph pattern) would
remove this ceiling; not done here since it's a bigger change than a
concurrency-safety fix warrants on its own.
"""

from __future__ import annotations

import asyncio
import importlib.util
import json
import os
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any

# Must be set before any crewai import touches its tracing listener --
# otherwise a first run in an interactive terminal blocks on a
# first-time "enable tracing?" prompt (confirmed while building this
# adapter). CI/production is always non-interactive, so this must never
# depend on TTY detection succeeding.
os.environ.setdefault("CREWAI_TRACING_ENABLED", "false")

from runkite_runner.adapter_checkpoint import (  # noqa: E402
    context_prompt_from_messages,
    decode_messages_checkpoint,
    encode_messages_checkpoint,
    merge_messages_input,
    messages_from_values_event,
)
from runkite_runner.usage import usage_from_metrics, values_with_usage
from runkite_runner.generic_worker import EventCallback, RunCancelled, make_event_factory, run_cancellable  # noqa: E402

from .otel_events import attach_otel_listeners  # noqa: E402


def _extract_text(result: Any) -> str:
    """CrewOutput (from kickoff/akickoff) exposes the final answer via
    .raw; fall back to str() for anything else a custom Crew might
    return."""
    if hasattr(result, "raw"):
        return result.raw
    if isinstance(result, str):
        return result
    return str(result)


class CrewAIAdapter:
    """Loads CrewAI Crews from a config file and executes them against
    the Runner Protocol. See module docstring for conventions."""

    checkpoint_framework = "crewai"

    def __init__(self) -> None:
        self.crews: dict[str, Any] = {}
        # Serializes concurrent akickoff() calls per graph_id -- see this
        # module's docstring for why a shared Crew instance isn't safe to
        # invoke concurrently. defaultdict so concurrent first-uses of a
        # graph_id can't race to create two different Lock objects for it.
        self._crew_locks: dict[str, asyncio.Lock] = defaultdict(asyncio.Lock)

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
            spec = importlib.util.spec_from_file_location(f"runkite_crewai.{graph_id}", str(abs_path))
            if spec is None or spec.loader is None:
                raise ValueError(f"Cannot load crew module: {abs_path}")
            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)

            export = getattr(module, export_name)
            # 0-arg factory support (building a Crew fresh per process
            # start, e.g. reading an API key at startup) -- same
            # convention as langchain_adapter/LangGraphAdapter.
            crew = export() if callable(export) and not hasattr(export, "kickoff") else export
            self.crews[graph_id] = crew

    async def execute(self, assignment: dict, event_callback: EventCallback, cancel_event) -> str:
        run_id = assignment["run_id"]
        graph_id = assignment["graph_id"]
        input_data = assignment.get("input") or {}
        make_event = make_event_factory(run_id)

        crew = self.crews.get(graph_id)
        if crew is None:
            await event_callback(
                make_event("error", {"message": f"unknown graph_id: {graph_id!r}. Available: {list(self.crews)}"})
            )
            return "error"

        await event_callback(make_event("lifecycle", {"event": "running"}))
        try:
            messages = list(input_data.get("messages", []))
            text = context_prompt_from_messages(messages)

            # See module docstring: a shared Crew instance's akickoff()
            # isn't safe to run concurrently with itself, so concurrent
            # runs on the same graph_id queue here rather than racing on
            # the crew's own instance attributes.
            # Shared bus listeners + ContextVar job state (see otel_events):
            # nests runkite.llm/tool under the active runkite.run; no-op when
            # OTEL is off. Safe across concurrent graph_ids / waiting lockers.
            with attach_otel_listeners(run_id):
                async with self._crew_locks[graph_id]:
                    result = await run_cancellable(crew.akickoff(inputs={"input": text}), cancel_event)
            reply = _extract_text(result)

            output_messages = messages + [{"role": "ai", "content": reply}]
            # CrewAI writes token totals onto the shared Crew after kickoff.
            usage = usage_from_metrics(getattr(crew, "usage_metrics", None))
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
