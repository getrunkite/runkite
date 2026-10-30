"""Managed Google ADK text turns with native sessions and durable receipts."""

from __future__ import annotations

import asyncio
import contextlib
import importlib.util
import inspect
import json
import logging
import sys
from collections.abc import AsyncIterable, Awaitable, Callable, Iterator
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from runkite_runner.checkpoint import resolve_checkpoint_http_url
from runkite_runner.completion import CompletionDeliveryError
from runkite_runner.generic_worker import EventCallback, RunCancelled, make_event_factory, run_cancellable
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient
from runkite_runner.usage import usage_or_unmetered, values_with_usage

from .state import FRAMEWORK, GoogleAdkState, dump_session, encode_state, native_usage, state_json

logger = logging.getLogger("runkite.runner.google_adk")
MAX_OUTPUT_BYTES = 3 << 20
MAX_RECEIPT_BYTES = 8 << 20


@dataclass(frozen=True)
class GoogleAdkRunContext:
    run_id: str
    thread_id: str
    tenant_id: str
    generation: int
    graph_id: str
    cancel_event: asyncio.Event


AgentFactory = Callable[[GoogleAdkRunContext], Any | Awaitable[Any]]


def _bounded(value: Any) -> None:
    if len(state_json(value).encode("utf-8")) > MAX_OUTPUT_BYTES:
        raise ValueError("Google ADK output exceeds the 3 MiB event limit")


def _message_payload(run_id: str, delta: str) -> list[Any]:
    value = [{"id": f"{run_id}:assistant", "type": "ai", "content": delta}, {}]
    _bounded(value)
    return value


def _incoming(value: Any) -> list[dict[str, str]]:
    if not isinstance(value, dict) or set(value) != {"messages"}:
        raise ValueError("managed Google ADK input contains only messages; native resume is unsupported")
    raw = value["messages"]
    if not isinstance(raw, list) or len(raw) != 1 or not isinstance(raw[0], dict):
        raise ValueError("managed Google ADK requires exactly one new user text message")
    message = raw[0]
    if (message.get("role") or message.get("type")) not in ("user", "human"):
        raise ValueError("managed Google ADK requires exactly one new user text message")
    text = message.get("content")
    if isinstance(text, list):
        if any(
            not isinstance(part, dict) or part.get("type") != "text" or not isinstance(part.get("text"), str)
            for part in text
        ):
            raise ValueError("managed Google ADK input supports text parts only")
        text = "".join(part["text"] for part in text)
    if not isinstance(text, str) or not text.strip():
        raise ValueError("the new user message must contain nonempty text")
    result = [{"role": "user", "content": text}]
    _bounded(result)
    return result


def _supported_agent(agent: Any) -> list[Any]:
    from google.adk.agents import LlmAgent
    from google.adk.models import BaseLlm
    from google.adk.tools import FunctionTool

    if type(agent) is not LlmAgent:
        raise ValueError("Google ADK factory must return a fresh ordinary LlmAgent")
    unsupported = (
        "parent_agent",
        "sub_agents",
        "input_schema",
        "output_schema",
        "state_schema",
        "planner",
        "code_executor",
        "retry_config",
        "mode",
        "parallel_worker",
        "rerun_on_resume",
        "wait_for_output",
        "before_agent_callback",
        "after_agent_callback",
        "before_model_callback",
        "after_model_callback",
        "on_model_error_callback",
        "before_tool_callback",
        "after_tool_callback",
        "on_tool_error_callback",
    )
    if any(getattr(agent, key) for key in unsupported):
        raise ValueError("Google ADK workflows, delegation, schemas, native resume and agent callbacks are unsupported")
    if not isinstance(agent.model, (str, BaseLlm)) or not agent.model:
        raise ValueError("Google ADK requires an explicit model name or native BaseLlm")
    if not isinstance(agent.instruction, str) or not isinstance(agent.global_instruction, str):
        raise ValueError("Google ADK dynamic instruction callbacks are unsupported")
    if agent.static_instruction is not None:
        from google.genai import types

        def text_instruction(value):
            if isinstance(value, str):
                return True
            if isinstance(value, types.Content):
                return all(text_instruction(part) for part in value.parts or [])
            if isinstance(value, types.Part):
                return set(value.model_dump(exclude_none=True)) <= {"text"}
            if isinstance(value, list):
                return all(text_instruction(part) for part in value)
            return False

        if not text_instruction(agent.static_instruction):
            raise ValueError("Google ADK static instructions support text only")
    config = agent.generate_content_config
    if config and (
        config.tools
        or config.response_schema
        or config.response_json_schema
        or config.response_mime_type not in (None, "text/plain")
    ):
        raise ValueError("Google ADK hosted tools and structured output modes are unsupported")
    tools = []
    for configured in agent.tools:
        tool = (
            configured
            if isinstance(configured, FunctionTool)
            else FunctionTool(configured)
            if callable(configured)
            else None
        )
        if tool is None or type(tool) is not FunctionTool or tool.is_long_running:
            raise ValueError("managed Google ADK supports ordinary local FunctionTools only")
        target = inspect.unwrap(tool.func)
        if inspect.isgeneratorfunction(target) or inspect.isasyncgenfunction(target):
            raise ValueError("Google ADK streaming tool results are unsupported")
        tools.append(tool)
    if len({tool.name for tool in tools}) != len(tools):
        raise ValueError("Google ADK local tool names must be unique")
    agent.tools = tools
    return tools


def _text(response: Any) -> str:
    result = []
    if response.content and response.content.role != "model":
        raise ValueError("Google ADK model responses must have native model content")
    for part in (response.content.parts if response.content else []) or []:
        fields = part.model_dump(mode="json", exclude_none=True)
        if any(key not in {"text", "thought", "thought_signature", "function_call", "part_metadata"} for key in fields):
            raise ValueError("Google ADK managed turns support text and local function calls only")
        if part.function_call is not None and not response.partial:
            call = part.function_call
            if call.partial_args is not None or call.will_continue or not call.name or type(call.args) is not dict:
                raise ValueError("Google ADK incomplete native function arguments are unsupported")
        if part.text and not part.thought:
            result.append(part.text)
    return "".join(result)


def _validate_actions(actions: Any) -> None:
    for key in (
        "artifact_delta",
        "transfer_to_agent",
        "transfer_reason",
        "escalate",
        "requested_auth_configs",
        "requested_tool_confirmations",
        "compaction",
        "end_of_agent",
        "agent_state",
        "rewind_before_invocation_id",
        "route",
        "render_ui_widgets",
        "set_model_response",
        "skip_summarization",
    ):
        if getattr(actions, key):
            raise ValueError("Google ADK native pending, delegation, artifacts and workflow actions are unsupported")
    if any(key.startswith(("app:", "user:")) for key in actions.state_delta):
        raise ValueError("Google ADK shared app/user state is unsupported")


async def _tool_result(value: Any) -> None:
    """Close unsupported lazy results even when ADK wraps a scalar result."""
    if isinstance(value, (AsyncIterable, Iterator)) or inspect.isawaitable(value):
        if hasattr(value, "aclose"):
            await value.aclose()
        elif hasattr(value, "close"):
            value.close()
        raise ValueError("Google ADK streaming tool results are unsupported")
    if isinstance(value, dict):
        for child in value.values():
            await _tool_result(child)
    elif isinstance(value, list):
        for child in value:
            await _tool_result(child)


class GoogleAdkAdapter:
    checkpoint_framework = ""

    def __init__(self) -> None:
        self.factories: dict[str, AgentFactory] = {}
        self._http_address = "http://localhost:2026"
        self._runner_kind = "python-google-adk"
        self._runner_token: str | None = None

    def configure(
        self, *, http_address: str, runner_kind: str = "python-google-adk", runner_token: str | None = None
    ) -> None:
        self._http_address, self._runner_kind, self._runner_token = http_address, runner_kind, runner_token

    def _state(self) -> GoogleAdkState:
        base = resolve_checkpoint_http_url(self._http_address)
        if not base:
            raise CompletionDeliveryError("Google ADK managed runs require RUNKITE_HTTP_URL")
        return GoogleAdkState(
            OpaqueCheckpointClient(
                http_base_url=base, framework=FRAMEWORK, runner_kind=self._runner_kind, runner_token=self._runner_token
            )
        )

    async def load_config(self, config_path: str) -> None:
        path = Path(config_path)
        config = json.loads(path.read_text())
        for dependency in config.get("dependencies", []):
            location = str((path.parent / dependency).resolve())
            if location not in sys.path:
                sys.path.insert(0, location)
        for graph_id, ref in config.get("graphs", {}).items():
            file, export = ref.split(":", 1)
            spec = importlib.util.spec_from_file_location(
                f"runkite_google_adk.{graph_id}", (path.parent / file).resolve()
            )
            if spec is None or spec.loader is None:
                raise ValueError("Cannot load Google ADK factory module")
            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)
            factory = getattr(module, export)
            if not callable(factory):
                raise ValueError("Google ADK exports must be per-run factories")
            self.factories[graph_id] = factory

    async def execute(self, assignment: dict, event_callback: EventCallback, cancel_event: asyncio.Event) -> str:
        from google.adk import Runner
        from google.adk.agents.run_config import RunConfig, StreamingMode
        from google.adk.plugins import BasePlugin
        from google.adk.sessions import Session
        from google.adk.telemetry.context import ContentCapturingMode, TelemetryConfig
        from google.genai import types

        from .sessions import RunLocalSessionService

        original_callback = event_callback
        delivery_error = None

        async def send(event):
            nonlocal delivery_error
            try:
                await original_callback(event)
            except CompletionDeliveryError as exc:
                # ADK wraps plugin exceptions. Preserve delivery failures so
                # the worker leaves this attempt recoverable without a status.
                delivery_error = exc
                raise

        event_callback = send
        run_id, graph_id = assignment["run_id"], assignment["graph_id"]
        thread_id, generation = assignment.get("thread_id") or "", int(assignment.get("generation") or 0)
        tenant_id = assignment.get("tenant_id") or "default"
        identity = {"graph_id": graph_id, "app_name": graph_id, "user_id": tenant_id, "session_id": thread_id}
        make_event = make_event_factory(run_id)

        async def error(exc):
            message = str(exc).encode("utf-8", errors="replace")[:65536].decode("utf-8", errors="ignore")
            await event_callback(make_event("error", {"message": message, "type": type(exc).__name__}))
            await event_callback(make_event("end", {"status": "error"}))

        try:
            if not thread_id:
                raise ValueError("thread_id is required")
            if assignment.get("checkpoint_ref") or assignment.get("resume_command") is not None:
                raise ValueError("Google ADK native resume and checkpoint_ref are unsupported")
            incoming = _incoming(assignment.get("input"))
            input_key = state_json(incoming)
            factory = self.factories.get(graph_id)
            if factory is None:
                raise ValueError(f"unknown graph_id: {graph_id!r}")
            allowed = assignment.get("allowed_tools")
            if allowed is not None and (
                not isinstance(allowed, list) or any(not isinstance(name, str) for name in allowed)
            ):
                raise ValueError("allowed_tools must be a list of tool names")
        except ValueError as exc:
            await error(exc)
            return "error"
        if cancel_event.is_set():
            await event_callback(make_event("end", {"status": "interrupted"}))
            return "interrupted"

        store, agent, runner, stream = self._state(), None, None, None
        calls: list[dict] = []
        usage, published_usage, started = None, None, False
        text_events: list[str] = []
        receipt_bytes = 2

        def check_cancel():
            if cancel_event.is_set():
                raise RunCancelled()

        def capture_usage():
            nonlocal usage
            reports = [call["usage"] for call in calls if call["usage"] is not None]
            if not reports:
                usage = usage_or_unmetered(None, started)
                return
            prompt = sum(report[0] for report in reports)
            completion = sum(report[1] for report in reports)
            usage = {"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion}
            models = {call["model"] for call in calls if call["usage"] is not None or call["complete"]}
            if len(models) == 1 and None not in models:
                usage["model"] = next(iter(models))

        def partial_values():
            return values_with_usage(
                {"google_adk": {"framework": FRAMEWORK, "generation": generation}}, usage_or_unmetered(usage, started)
            )

        async def publish_usage():
            nonlocal published_usage
            capture_usage()
            current = partial_values().get("usage")
            if current is not None and current != published_usage:
                value = partial_values()
                _bounded(value)
                await event_callback(make_event("values", value))
                published_usage = dict(current)

        async def emit_text(delta):
            nonlocal receipt_bytes
            if not delta:
                return
            payload = _message_payload(run_id, delta)
            receipt_bytes += len(state_json(delta).encode()) + bool(text_events)
            if receipt_bytes > MAX_RECEIPT_BYTES:
                raise ValueError("Google ADK text receipt exceeds the 8 MiB limit")
            text_events.append(delta)
            await event_callback(make_event("messages", payload))

        class ManagedPlugin(BasePlugin):
            def __init__(self):
                super().__init__(name="runkite-managed")

            async def before_model_callback(self, *, callback_context, llm_request):
                nonlocal started
                check_cancel()
                _validate_actions(callback_context.actions)
                calls.append(
                    {
                        "usage": None,
                        "model": None,
                        "models": set(),
                        "complete": False,
                        "partial": "",
                        "text": "",
                        "calls": False,
                    }
                )
                started = True

            async def after_model_callback(self, *, callback_context, llm_response):
                call = calls[-1]
                metadata = llm_response.usage_metadata
                if metadata is not None:
                    call["usage"] = native_usage(metadata)
                if isinstance(llm_response.model_version, str) and llm_response.model_version.strip():
                    call["models"].add(llm_response.model_version)
                    call["model"] = llm_response.model_version if len(call["models"]) == 1 else None
                await publish_usage()
                check_cancel()
                if llm_response.error_code or llm_response.error_message or llm_response.interrupted:
                    raise ValueError("Google ADK model returned an error or interrupted response")
                if llm_response.finish_reason not in (None, types.FinishReason.STOP):
                    raise ValueError("Google ADK model did not finish a supported text turn")
                text = _text(llm_response)
                if llm_response.partial:
                    call["partial"] += text
                    await emit_text(text)
                    return
                if llm_response.turn_complete is False or llm_response.interaction_status not in (
                    None,
                    types.InteractionStatus.IDLE,
                ):
                    raise ValueError("Google ADK returned an unfinished native model turn")
                if call["complete"]:
                    raise ValueError("Google ADK emitted multiple completed responses for one model call")
                if not text.startswith(call["partial"]):
                    raise ValueError("Google ADK completed text rewrites streamed fragments")
                await emit_text(text[len(call["partial"]) :])
                call["text"] = text
                call["complete"] = True
                call["calls"] = bool(
                    llm_response.content and any(part.function_call for part in llm_response.content.parts or [])
                )
                _validate_actions(callback_context.actions)

            async def before_tool_callback(self, *, tool, tool_args, tool_context):
                check_cancel()
                await publish_usage()
                if tool not in local_tools:
                    raise ValueError("Google ADK attempted an unsupported nonlocal tool")
                payload = {"id": tool_context.function_call_id, "name": tool.name, "args": tool_args}
                _bounded(payload)
                await event_callback(make_event("tool_call", payload))
                if allowed is not None and tool.name not in allowed:
                    await event_callback(
                        make_event(
                            "tool_auth",
                            {
                                "stage": "tool.call",
                                "effect": "deny",
                                "tool": tool.name,
                                "reason": "tool is not permitted by allowed_tools",
                                "reason_code": "tool_not_allowed",
                                "generation": generation,
                            },
                        )
                    )
                    raise ValueError(f"Tool {tool.name!r} is not permitted by allowed_tools")
                if await tool.check_require_confirmation(tool_args, tool_context):
                    raise ValueError("Google ADK native confirmations are unsupported")
                _validate_actions(tool_context.actions)
                check_cancel()

            async def after_tool_callback(self, *, tool, tool_args, tool_context, result):
                await _tool_result(result)
                _bounded(result)
                _validate_actions(tool_context.actions)
                check_cancel()

        try:
            prior, etag = await store.load(thread_id, **identity)
            check_cancel()
            await event_callback(make_event("lifecycle", {"event": "running"}))
            completed = (prior or {}).get("completed")
            if completed and completed["run_id"] == run_id:
                if completed["input"] != input_key:
                    raise ValueError("reclaimed run input differs from its committed turn")
                output = json.loads(state_json(completed["output"]))
                output["google_adk"]["generation"] = generation
                _bounded(output)
                if len(state_json(completed["text_events"]).encode()) > MAX_RECEIPT_BYTES:
                    raise ValueError("Google ADK text receipt exceeds the 8 MiB limit")
                for delta in completed["text_events"]:
                    _message_payload(run_id, delta)
                for delta in completed["text_events"]:
                    check_cancel()
                    await event_callback(make_event("messages", _message_payload(run_id, delta)))
                await event_callback(make_event("values", output))
                await event_callback(make_event("end", {"status": "success"}))
                return "success"
            context = GoogleAdkRunContext(run_id, thread_id, tenant_id, generation, graph_id, cancel_event)

            async def build():
                check_cancel()
                result = factory(context)
                return await result if inspect.isawaitable(result) else result

            agent = await run_cancellable(build(), cancel_event)
            local_tools = _supported_agent(agent)
            session = Session.model_validate_json(state_json(prior["history"])) if prior else None
            sessions = RunLocalSessionService(
                app_name=graph_id, user_id=tenant_id, session_id=thread_id, session=session
            )
            if session is None:
                await sessions.create_session(app_name=graph_id, user_id=tenant_id, session_id=thread_id)
            runner = Runner(app_name=graph_id, agent=agent, session_service=sessions, plugins=[ManagedPlugin()])
            run_config = RunConfig(
                streaming_mode=StreamingMode.SSE,
                telemetry=TelemetryConfig(
                    capture_message_content=ContentCapturingMode.NO_CONTENT,
                    adk_experimental_telemetry_opt_in=False,
                    experimental_features_opt_in=frozenset(),
                ),
            )
            stream = runner.run_async(
                user_id=tenant_id,
                session_id=thread_id,
                invocation_id=run_id,
                new_message=types.Content(role="user", parts=[types.Part(text=incoming[0]["content"])]),
                run_config=run_config,
                abort_signal=cancel_event,
            )
            while True:
                try:
                    event = await run_cancellable(anext(stream), cancel_event)
                except StopAsyncIteration:
                    break
                check_cancel()
                if event.invocation_id != run_id:
                    raise ValueError("Google ADK returned an event for another invocation")
                _validate_actions(event.actions)
                if event.error_code or event.error_message or event.interrupted or event.long_running_tool_ids:
                    raise ValueError("Google ADK did not complete a supported native invocation")
            check_cancel()
            if (
                not calls
                or any(not call["complete"] for call in calls)
                or calls[-1]["calls"]
                or not calls[-1]["text"].strip()
            ):
                raise ValueError("Google ADK invocation has no settled final text response")
            capture_usage()
            await publish_usage()
            messages = [
                *((prior or {}).get("messages") or []),
                *incoming,
                {"role": "assistant", "content": "".join(text_events)},
            ]
            output = values_with_usage(
                {"messages": messages, "google_adk": {"framework": FRAMEWORK, "generation": generation}},
                usage_or_unmetered(usage, started),
            )
            _bounded(output)
            envelope = {
                "v": 1,
                "framework": FRAMEWORK,
                "graph_id": graph_id,
                "history": dump_session(await sessions.snapshot()),
                "messages": messages,
                "completed": {
                    "status": "success",
                    "run_id": run_id,
                    "input": input_key,
                    "output": output,
                    "text_events": text_events,
                },
            }
            encode_state(envelope, **identity)
            await store.save(thread_id, envelope, etag, **identity)
            await event_callback(make_event("values", output))
            await event_callback(make_event("end", {"status": "success"}))
            return "success"
        except RunCancelled:
            await event_callback(make_event("values", partial_values()))
            await event_callback(make_event("end", {"status": "interrupted"}))
            return "interrupted"
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            if delivery_error is not None:
                raise delivery_error from exc
            if started or usage:
                capture_usage()
                await event_callback(make_event("values", partial_values()))
            if cancel_event.is_set():
                await event_callback(make_event("end", {"status": "interrupted"}))
                return "interrupted"
            logger.exception("Google ADK run failed (%s)", type(exc).__name__)
            await error(exc)
            return "error"
        finally:
            if stream is not None:
                with contextlib.suppress(Exception):
                    await stream.aclose()
            if runner is not None:
                try:
                    await runner.close()
                except Exception as exc:
                    logger.warning("Google ADK runner close failed (%s)", type(exc).__name__)
            from google.adk.models.google_llm import Gemini

            model = getattr(agent, "model", None)
            if isinstance(model, Gemini) and model.client is not None:
                # Explicit clients belong to this per-run factory. Do not
                # materialize or reach into ADK's SDK-owned lazy client cache.
                try:
                    await model.client.aio.aclose()
                except Exception as exc:
                    logger.warning("Google ADK async client close failed (%s)", type(exc).__name__)
                try:
                    model.client.close()
                except Exception as exc:
                    logger.warning("Google ADK client close failed (%s)", type(exc).__name__)
            try:
                await store.aclose()
            except Exception as exc:
                logger.warning("Google ADK state close failed (%s)", type(exc).__name__)
