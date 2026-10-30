"""Managed Microsoft Agent Framework adapter for the Runkite Runner Protocol.

Factories build a public ``Agent`` per run. Native ``AgentSession`` history and
committed-turn receipts live in run-bound opaque checkpoints with CAS, matching
the managed AI SDK safeguards: reclaim of the same run_id replays without a
second model or tool cycle.
"""

from __future__ import annotations

import asyncio
import importlib.util
import json
import logging
import sys
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from runkite_runner.checkpoint import resolve_checkpoint_http_url
from runkite_runner.completion import CompletionDeliveryError
from runkite_runner.generic_worker import EventCallback, RunCancelled, make_event_factory, run_cancellable
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient
from runkite_runner.usage import usage_or_unmetered, usage_payload, values_with_usage

from .state import FRAMEWORK, AgentFrameworkState, state_json

logger = logging.getLogger("runkite.runner.agent_framework")

AgentFactory = Callable[["AgentFrameworkRunContext"], Any | Awaitable[Any]]
MAX_OUTPUT_BYTES = 3 << 20
MAX_RECEIPT_BYTES = 8 << 20


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
    if not isinstance(input_data, dict):
        raise ValueError("input must be an object")
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
            if any(
                not isinstance(part, dict) or part.get("type") != "text" or not isinstance(part.get("text"), str)
                for part in content
            ):
                raise ValueError("managed Agent Framework messages support text parts only")
            content = "".join(part["text"] for part in content)
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
        if getattr(content, "type", None) == "text" and isinstance(text, str) and text:
            parts.append(text)
    return "".join(parts)


def _assistant_message(text: str) -> dict[str, str]:
    return {"role": "assistant", "content": text}


def _bounded_output(output: dict[str, Any]) -> None:
    if len(state_json(output).encode("utf-8")) > MAX_OUTPUT_BYTES:
        raise ValueError("Agent Framework output exceeds the 3 MiB event limit")


def _message_payload(run_id: str, delta: str) -> list[dict[str, str]]:
    payload = [{"id": f"{run_id}:assistant", "type": "ai", "content": delta}, {}]
    if len(state_json(payload).encode("utf-8")) > MAX_OUTPUT_BYTES:
        raise ValueError("Agent Framework message exceeds the 3 MiB event limit")
    return payload


def _observed_usage(current: dict[str, Any] | None, update: Any) -> dict[str, Any] | None:
    """Match native usage append/replace semantics without retaining updates."""
    for content in getattr(update, "contents", None) or []:
        if getattr(content, "type", None) != "usage":
            continue
        usage = _usage_from_details(getattr(content, "usage_details", None))
        if getattr(content, "merge_mode", None) == "replace":
            current = usage
        elif usage:
            current = {
                key: (current or {}).get(key, 0) + usage[key]
                for key in ("prompt_tokens", "completion_tokens", "total_tokens")
            }
    return current


def _require_completed_native_response(response: Any) -> None:
    if getattr(response, "user_input_requests", None):
        raise ValueError("Agent Framework native approval and user-input continuation are unsupported")
    if getattr(response, "continuation_token", None) is not None:
        raise ValueError("Agent Framework native background continuation is unsupported")
    unresolved: set[str | None] = set()
    for message in getattr(response, "messages", None) or []:
        for content in getattr(message, "contents", None) or []:
            if getattr(content, "type", None) == "function_call" and not getattr(content, "informational_only", False):
                unresolved.add(getattr(content, "call_id", None))
            elif getattr(content, "type", None) == "function_result":
                unresolved.discard(getattr(content, "call_id", None))
    if unresolved:
        raise ValueError("Agent Framework turn has unresolved native tool calls")


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

    async def execute(self, assignment: dict, event_callback: EventCallback, cancel_event: asyncio.Event) -> str:
        from agent_framework import Agent, AgentSession, Content, Message

        run_id = assignment["run_id"]
        graph_id = assignment["graph_id"]
        thread_id = assignment.get("thread_id") or ""
        generation = int(assignment.get("generation") or 0)
        tenant_id = assignment.get("tenant_id") or "default"
        make_event = make_event_factory(run_id)
        if not thread_id:
            await event_callback(make_event("error", {"message": "thread_id is required", "type": "InvalidAssignment"}))
            return "error"
        if assignment.get("checkpoint_ref") or assignment.get("resume_command") is not None:
            await event_callback(
                make_event(
                    "error",
                    {
                        "message": "checkpoint_ref and native resume are unsupported for python-agent-framework",
                        "type": "UnsupportedContinuation",
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
        if cancel_event.is_set():
            await event_callback(make_event("end", {"status": "interrupted"}))
            return "interrupted"

        state_store = self._state()
        agent = None
        stream = None
        usage: dict[str, Any] | None = None
        published_usage: dict[str, Any] | None = None
        produced = False
        started = False
        session = None

        async def close_stream() -> None:
            nonlocal stream
            current, stream = stream, None
            if current is not None:
                try:
                    await current.close()
                except Exception as exc:
                    logger.warning("Agent Framework stream close failed (%s)", type(exc).__name__)

        def partial_values() -> dict[str, Any]:
            return values_with_usage(
                {
                    "agent_framework": {
                        "framework": FRAMEWORK,
                        "generation": generation,
                        **({"session_id": session.session_id} if session is not None else {}),
                    }
                },
                usage_or_unmetered(usage, produced or started),
            )

        async def publish_observed_usage() -> None:
            nonlocal published_usage
            values = partial_values()
            current = values.get("usage")
            if current is not None and current != published_usage:
                _bounded_output(values)
                await event_callback(make_event("values", values))
                published_usage = dict(current)

        try:
            prior, etag = await state_store.load(thread_id, graph_id=graph_id)
            if cancel_event.is_set():
                await event_callback(make_event("end", {"status": "interrupted"}))
                return "interrupted"
            await event_callback(make_event("lifecycle", {"event": "running"}))
            completed = prior.get("completed") if isinstance(prior, dict) else None
            if isinstance(completed, dict) and completed.get("run_id") == run_id:
                if completed["input"] != input_key:
                    raise ValueError("reclaimed run input differs from its committed turn")
                # Validate every replay payload before exposing any saved text.
                output = json.loads(state_json(completed["output"]))
                output["agent_framework"]["generation"] = generation
                _bounded_output(output)
                text_events = completed["text_events"]
                if len(state_json(text_events).encode("utf-8")) > MAX_RECEIPT_BYTES:
                    raise ValueError("Agent Framework text receipt exceeds the 8 MiB limit")
                for delta in text_events:
                    _message_payload(run_id, delta)
                for delta in text_events:
                    if cancel_event.is_set():
                        raise RunCancelled()
                    if delta:
                        await event_callback(make_event("messages", _message_payload(run_id, delta)))
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

            async def build_agent():
                if cancel_event.is_set():
                    raise RunCancelled()
                built = factory(context)
                return await built if hasattr(built, "__await__") else built

            agent = await run_cancellable(build_agent(), cancel_event)
            if not isinstance(agent, Agent):
                raise ValueError("Agent Framework factory must return an agent_framework.Agent")
            if cancel_event.is_set():
                raise RunCancelled()
            await run_cancellable(agent.open(), cancel_event)
            if cancel_event.is_set():
                raise RunCancelled()
            session_data = prior.get("session") if isinstance(prior, dict) else None
            session = AgentSession.from_dict(session_data) if isinstance(session_data, dict) else agent.create_session()
            native_input = [
                Message(role=message["role"], contents=[Content.from_text(message["content"])]) for message in incoming
            ]
            text_events: list[str] = []
            assistant_text = ""
            receipt_bytes = 2

            async def emit_text(delta: str) -> None:
                nonlocal assistant_text, receipt_bytes, produced
                payload = _message_payload(run_id, delta)
                encoded_size = len(state_json(delta).encode("utf-8"))
                receipt_bytes += encoded_size + (1 if text_events else 0)
                if receipt_bytes > MAX_RECEIPT_BYTES:
                    raise ValueError("Agent Framework text receipt exceeds the 8 MiB limit")
                produced = True
                assistant_text += delta
                text_events.append(delta)
                await event_callback(make_event("messages", payload))

            async def run_turn() -> Any:
                nonlocal stream, usage, produced, started
                if cancel_event.is_set():
                    raise RunCancelled()
                stream = agent.run(native_input, stream=True, session=session)
                started = True
                try:
                    async for update in stream:
                        # Usage belongs to work already done. Capture it before
                        # observing a cancel that arrived with this update.
                        usage = _observed_usage(usage, update)
                        produced = produced or bool(getattr(update, "contents", None))
                        # Cancellation can finalize on the plane before a later
                        # ReportStatus. Publish the observed snapshot now; these
                        # values do not settle or bill the run themselves.
                        await publish_observed_usage()
                        if cancel_event.is_set():
                            raise RunCancelled()
                        if getattr(update, "user_input_requests", None):
                            raise ValueError(
                                "Agent Framework native approval and user-input continuation are unsupported"
                            )
                        if getattr(update, "merge_mode", None) == "replace":
                            raise ValueError("Agent Framework replacement text updates are unsupported")
                        delta = _text_from_update(update)
                        if delta:
                            await emit_text(delta)
                    return await stream.get_final_response()
                finally:
                    await close_stream()

            final = await run_cancellable(run_turn(), cancel_event)
            # Native final usage already aggregates streamed usage. Replace,
            # never add it a second time; retain observed usage if it is absent.
            usage = _usage_from_details(getattr(final, "usage_details", None)) or usage
            await publish_observed_usage()
            _require_completed_native_response(final)
            if cancel_event.is_set():
                raise RunCancelled()
            final_text = str(getattr(final, "text", "") or "")
            if not assistant_text and final_text:
                await emit_text(final_text)
            elif assistant_text != final_text:
                raise ValueError("streamed text differs from the completed native response")
            usage = usage_or_unmetered(usage, produced or started)
            history = [*((prior or {}).get("messages") or []), *incoming]
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
            _bounded_output(output)
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
            await state_store.save(thread_id, receipt, etag, graph_id=graph_id)
            await event_callback(make_event("values", output))
            await event_callback(make_event("end", {"status": "success"}))
            return "success"
        except RunCancelled:
            # A terminal cancel has fenced checkpoint writes already. Keep the
            # previous committed turn rather than persisting unfinished tools.
            await event_callback(make_event("values", partial_values()))
            await event_callback(make_event("end", {"status": "interrupted"}))
            return "interrupted"
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            logger.exception("Agent Framework run failed: %s", exc)
            if usage or produced or started:
                await event_callback(make_event("values", partial_values()))
            await event_callback(make_event("error", {"message": str(exc), "type": type(exc).__name__}))
            return "error"
        finally:
            try:
                await close_stream()
            finally:
                try:
                    if isinstance(agent, Agent):
                        await agent.close()
                except Exception as exc:
                    logger.warning("Agent Framework agent close failed (%s)", type(exc).__name__)
                finally:
                    try:
                        await state_store.aclose()
                    except Exception as exc:
                        logger.warning("Agent Framework state client close failed (%s)", type(exc).__name__)
