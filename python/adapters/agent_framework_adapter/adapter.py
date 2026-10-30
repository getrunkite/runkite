"""Managed Microsoft Agent Framework adapter for the Runkite Runner Protocol.

Factories build a public ``Agent`` per run. Native ``AgentSession`` history and
committed-turn receipts live in run-bound opaque checkpoints with CAS, matching
the managed AI SDK safeguards: reclaim of the same run_id replays without a
second model or tool cycle.
"""

from __future__ import annotations

import importlib.util
import json
import logging
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Awaitable, Callable

from runkite_runner.checkpoint import resolve_checkpoint_http_url
from runkite_runner.completion import CompletionDeliveryError
from runkite_runner.generic_worker import EventCallback, RunCancelled, make_event_factory, run_cancellable
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient
from runkite_runner.usage import usage_or_unmetered, usage_payload, values_with_usage

from .state import FRAMEWORK, AgentFrameworkState, state_json

logger = logging.getLogger("runkite.runner.agent_framework")

AgentFactory = Callable[["AgentFrameworkRunContext"], Any | Awaitable[Any]]


@dataclass(frozen=True)
class AgentFrameworkRunContext:
    run_id: str
    thread_id: str
    tenant_id: str
    generation: int
    graph_id: str
    cancel_event: Any


def _input_key(messages: list[Any]) -> str:
    return state_json(messages)


def _user_messages(input_data: dict) -> list[dict[str, Any]]:
    raw = input_data.get("messages")
    if not isinstance(raw, list) or not raw:
        raise ValueError("input.messages is required")
    out: list[dict[str, Any]] = []
    for item in raw:
        if not isinstance(item, dict):
            raise ValueError("each input message must be an object")
        role = item.get("role") or item.get("type")
        content = item.get("content")
        if role not in ("user", "human", "system"):
            raise ValueError("managed Agent Framework turns accept user/system messages only")
        if isinstance(content, list):
            content = "".join(
                part.get("text", "")
                for part in content
                if isinstance(part, dict) and part.get("type") == "text"
            )
        if not isinstance(content, str):
            raise ValueError("message content must be text")
        out.append({"role": "user" if role == "human" else role, "content": content})
    if out[-1]["role"] != "user":
        raise ValueError("the newest message must be a user turn")
    if not out[-1]["content"].strip():
        raise ValueError("the newest user message is empty")
    return out


def _usage_from_details(details: Any) -> dict[str, Any] | None:
    if not isinstance(details, dict):
        return None
    prompt = int(details.get("input_token_count") or 0)
    completion = int(details.get("output_token_count") or 0)
    total = int(details.get("total_token_count") or (prompt + completion))
    if not prompt and not completion and not total:
        return None
    payload = {
        "prompt_tokens": prompt or max(total - completion, 0),
        "completion_tokens": completion,
        "total_tokens": total or (prompt + completion),
    }
    return usage_payload(payload)


def _text_from_update(update: Any) -> str:
    parts: list[str] = []
    for content in getattr(update, "contents", None) or []:
        text = getattr(content, "text", None)
        if isinstance(text, str) and text:
            parts.append(text)
    return "".join(parts)


def _assistant_message(text: str) -> dict[str, str]:
    return {"role": "assistant", "content": text}


class AgentFrameworkAdapter:
    """Loads Agent factories and executes managed Microsoft Agent Framework runs."""

    # Own CAS receipts inside execute; do not also opt into generic prepare/serialize.
    checkpoint_framework = ""

    def __init__(self) -> None:
        self.factories: dict[str, AgentFactory] = {}
        self._http_address = "http://localhost:2026"
        self._runner_kind = "python-agent-framework"
        self._runner_token: str | None = None

    def configure(
        self,
        *,
        http_address: str,
        runner_kind: str = "python-agent-framework",
        runner_token: str | None = None,
    ) -> None:
        self._http_address = http_address
        self._runner_kind = runner_kind
        self._runner_token = runner_token

    def _state(self) -> AgentFrameworkState:
        base = resolve_checkpoint_http_url(self._http_address)
        if not base:
            raise CompletionDeliveryError("Agent Framework managed runs require RUNKITE_HTTP_URL")
        return AgentFrameworkState(
            OpaqueCheckpointClient(
                http_base_url=base,
                framework=FRAMEWORK,
                runner_kind=self._runner_kind,
                runner_token=self._runner_token,
            )
        )

    async def load_config(self, config_path: str) -> None:
        path = Path(config_path)
        with open(path) as handle:
            config = json.load(handle)
        config_dir = path.parent
        for dep in config.get("dependencies", []):
            dep_path = (config_dir / dep).resolve()
            if str(dep_path) not in sys.path:
                sys.path.insert(0, str(dep_path))
        for graph_id, ref in config.get("graphs", {}).items():
            file_path, export_name = ref.split(":", 1)
            abs_path = (config_dir / file_path).resolve()
            spec = importlib.util.spec_from_file_location(f"runkite_agent_framework.{graph_id}", str(abs_path))
            if spec is None or spec.loader is None:
                raise ValueError(f"Cannot load Agent Framework module: {abs_path}")
            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)
            export = getattr(module, export_name)
            if not callable(export):
                raise ValueError(f"Agent Framework export {export_name!r} must be a factory callable")
            self.factories[graph_id] = export

    async def execute(self, assignment: dict, event_callback: EventCallback, cancel_event) -> str:
        from agent_framework import Agent, AgentSession

        run_id = assignment["run_id"]
        graph_id = assignment["graph_id"]
        thread_id = assignment.get("thread_id") or ""
        generation = int(assignment.get("generation") or 0)
        tenant_id = assignment.get("tenant_id") or "default"
        make_event = make_event_factory(run_id)
        if not thread_id:
            await event_callback(make_event("error", {"message": "thread_id is required", "type": "InvalidAssignment"}))
            return "error"
        if assignment.get("checkpoint_ref"):
            await event_callback(
                make_event(
                    "error",
                    {
                        "message": "checkpoint_ref is unsupported for python-agent-framework",
                        "type": "CheckpointRefUnsupported",
                    },
                )
            )
            return "error"

        factory = self.factories.get(graph_id)
        if factory is None:
            await event_callback(
                make_event("error", {"message": f"unknown graph_id: {graph_id!r}", "type": "UnknownAgent"})
            )
            return "error"

        try:
            incoming = _user_messages(assignment.get("input") or {})
            input_key = _input_key(incoming)
        except ValueError as exc:
            await event_callback(make_event("error", {"message": str(exc), "type": type(exc).__name__}))
            return "error"

        state_store = self._state()
        try:
            prior, etag = await state_store.load(thread_id)
        except CompletionDeliveryError:
            raise

        await event_callback(make_event("lifecycle", {"event": "running"}))

        completed = prior.get("completed") if isinstance(prior, dict) else None
        if isinstance(completed, dict) and completed.get("run_id") == run_id:
            if completed.get("input") != input_key:
                await event_callback(
                    make_event(
                        "error",
                        {
                            "message": "reclaimed run input differs from its committed turn",
                            "type": "ReceiptMismatch",
                        },
                    )
                )
                return "error"
            for delta in completed.get("text_events") or []:
                if isinstance(delta, str) and delta:
                    await event_callback(
                        make_event("messages", [{"id": f"{run_id}:assistant", "type": "ai", "content": delta}, {}])
                    )
            output = completed.get("output") if isinstance(completed.get("output"), dict) else {}
            await event_callback(make_event("values", output))
            await event_callback(make_event("end", {"status": "success"}))
            return "success"

        context = AgentFrameworkRunContext(
            run_id=run_id,
            thread_id=thread_id,
            tenant_id=tenant_id,
            generation=generation,
            graph_id=graph_id,
            cancel_event=cancel_event,
        )
        try:
            built = factory(context)
            agent = await built if hasattr(built, "__await__") else built
        except Exception as exc:
            await event_callback(make_event("error", {"message": str(exc), "type": type(exc).__name__}))
            return "error"
        if not isinstance(agent, Agent):
            await event_callback(
                make_event(
                    "error",
                    {
                        "message": "Agent Framework factory must return an agent_framework.Agent",
                        "type": "InvalidFactory",
                    },
                )
            )
            return "error"

        session_data = prior.get("session") if isinstance(prior, dict) else None
        session = AgentSession.from_dict(session_data) if isinstance(session_data, dict) else agent.create_session()

        newest = incoming[-1]["content"]
        text_events: list[str] = []
        assistant_text = ""
        usage: dict[str, Any] | None = None
        produced = False

        async def _run_turn() -> Any:
            nonlocal assistant_text, usage, produced
            stream = agent.run(newest, stream=True, session=session)
            async for update in stream:
                if cancel_event.is_set():
                    raise RunCancelled()
                delta = _text_from_update(update)
                if delta:
                    produced = True
                    assistant_text += delta
                    text_events.append(delta)
                    await event_callback(
                        make_event("messages", [{"id": f"{run_id}:assistant", "type": "ai", "content": delta}, {}])
                    )
            return await stream.get_final_response()

        try:
            final = await run_cancellable(_run_turn(), cancel_event)
            usage = _usage_from_details(getattr(final, "usage_details", None))
            if not assistant_text:
                assistant_text = str(getattr(final, "text", "") or "")
                if assistant_text:
                    produced = True
                    text_events.append(assistant_text)
                    await event_callback(
                        make_event(
                            "messages",
                            [{"id": f"{run_id}:assistant", "type": "ai", "content": assistant_text}, {}],
                        )
                    )
            usage = usage_or_unmetered(usage, produced)
            history: list[dict[str, Any]] = []
            if isinstance(prior, dict) and isinstance(prior.get("messages"), list):
                history.extend(prior["messages"])
            history.extend(incoming)
            if assistant_text:
                history.append(_assistant_message(assistant_text))
            output = values_with_usage(
                {
                    "messages": history,
                    "agent_framework": {
                        "framework": FRAMEWORK,
                        "generation": generation,
                        "session_id": session.session_id,
                    },
                },
                usage,
            )
            receipt = {
                "v": 1,
                "framework": FRAMEWORK,
                "graph_id": graph_id,
                "session": session.to_dict(),
                "messages": history,
                "completed": {
                    "run_id": run_id,
                    "input": input_key,
                    "status": "success",
                    "output": output,
                    "text_events": text_events,
                },
            }
            await state_store.save(thread_id, receipt, etag)
            await event_callback(make_event("values", output))
            await event_callback(make_event("end", {"status": "success"}))
            return "success"
        except RunCancelled:
            usage = usage_or_unmetered(usage, produced)
            await event_callback(
                make_event(
                    "values",
                    values_with_usage(
                        {
                            "agent_framework": {
                                "framework": FRAMEWORK,
                                "generation": generation,
                                "session_id": session.session_id,
                            }
                        },
                        usage,
                    ),
                )
            )
            # Persist session progress without a success receipt so a retry can continue.
            interrupted_state = {
                "v": 1,
                "framework": FRAMEWORK,
                "graph_id": graph_id,
                "session": session.to_dict(),
                "messages": (prior or {}).get("messages") if isinstance(prior, dict) else [],
            }
            try:
                await state_store.save(thread_id, interrupted_state, etag)
            except CompletionDeliveryError:
                raise
            await event_callback(make_event("end", {"status": "interrupted"}))
            return "interrupted"
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            logger.exception("Agent Framework run failed: %s", exc)
            await event_callback(make_event("error", {"message": str(exc), "type": type(exc).__name__}))
            return "error"
