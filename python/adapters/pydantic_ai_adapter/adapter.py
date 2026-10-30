"""Managed Pydantic AI text and validated JSON turns over Runner Protocol."""

from __future__ import annotations

import asyncio
import contextlib
import importlib.util
import inspect
import json
import logging
import sys
from collections.abc import AsyncIterable, Awaitable, Callable, Iterator
from dataclasses import dataclass, fields, is_dataclass
from enum import Enum
from pathlib import Path
from typing import Any, get_args, get_origin

from runkite_runner.checkpoint import resolve_checkpoint_http_url
from runkite_runner.completion import CompletionDeliveryError
from runkite_runner.generic_worker import EventCallback, RunCancelled, make_event_factory, run_cancellable
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient
from runkite_runner.usage import usage_or_unmetered, values_with_usage

from .state import FRAMEWORK, MAX_STATE_BYTES, PydanticAIState, dump_conversation, encode_state, state_json

logger = logging.getLogger("runkite.runner.pydantic_ai")
MAX_OUTPUT_BYTES = 3 << 20
MAX_RECEIPT_BYTES = 8 << 20


@dataclass(frozen=True)
class PydanticAIRunContext:
    run_id: str
    thread_id: str
    tenant_id: str
    generation: int
    graph_id: str
    cancel_event: asyncio.Event


@dataclass(frozen=True)
class PydanticAIAgentDefinition:
    """A fresh native agent and optional server-owned dependencies for one run."""

    agent: Any
    deps: Any = None


AgentFactory = Callable[[PydanticAIRunContext], Any | Awaitable[Any]]


def _bounded(value: Any) -> None:
    if len(state_json(value).encode("utf-8")) > MAX_OUTPUT_BYTES:
        raise ValueError("Pydantic AI output exceeds the 3 MiB event limit")


def _message_payload(run_id: str, delta: str) -> list[Any]:
    payload = [{"id": f"{run_id}:assistant", "type": "ai", "content": delta}, {}]
    _bounded(payload)
    return payload


def _incoming(value: Any) -> list[dict[str, str]]:
    if not isinstance(value, dict) or set(value) != {"messages"}:
        raise ValueError("managed Pydantic AI input contains only messages; native resume is unsupported")
    raw = value["messages"]
    if not isinstance(raw, list) or not raw:
        raise ValueError("input.messages is required")
    messages = []
    for item in raw:
        if not isinstance(item, dict):
            raise ValueError("each input message must be an object")
        role, content = item.get("role") or item.get("type"), item.get("content")
        if role not in ("user", "human", "system"):
            raise ValueError("managed Pydantic AI accepts new user/system text messages only")
        if isinstance(content, list):
            if any(
                not isinstance(part, dict) or part.get("type") != "text" or not isinstance(part.get("text"), str)
                for part in content
            ):
                raise ValueError("managed Pydantic AI input supports text parts only")
            content = "".join(part["text"] for part in content)
        if not isinstance(content, str):
            raise ValueError("message content must be text")
        messages.append({"role": "user" if role == "human" else role, "content": content})
    if messages[-1]["role"] != "user" or not messages[-1]["content"].strip():
        raise ValueError("the newest message must be a nonempty user turn")
    _bounded(messages)
    return messages


def _plain_output_type(spec: Any) -> None:
    from pydantic_ai import DeferredToolRequests

    if spec is DeferredToolRequests:
        raise ValueError("Pydantic AI native deferred tools and approvals are unsupported")
    if isinstance(spec, (list, tuple)):
        for item in spec:
            _plain_output_type(item)
    elif get_origin(spec) is not None:
        for item in get_args(spec):
            if item is DeferredToolRequests:
                raise ValueError("Pydantic AI native deferred tools and approvals are unsupported")
    elif spec is not None and not isinstance(spec, type):
        raise ValueError("Pydantic AI output functions and custom output mode markers are unsupported")


def _supported_model(model: Any) -> None:
    from pydantic_ai.models.fallback import FallbackModel

    if isinstance(model, FallbackModel) or type(model).__module__.endswith(".instrumented"):
        raise ValueError("Pydantic AI fallback and explicitly instrumented model wrappers are unsupported")


def _supported_agent(agent: Any) -> None:
    from pydantic_ai import Agent
    from pydantic_ai.toolsets import FunctionToolset

    if not isinstance(agent, Agent):
        raise ValueError("Pydantic AI factory must return a pydantic_ai.Agent")
    _plain_output_type(agent.output_type)
    _supported_model(agent.model)
    for toolset in agent.toolsets:
        if not isinstance(toolset, FunctionToolset):
            raise ValueError("managed Pydantic AI supports ordinary local function toolsets only")
        for tool in toolset.tools.values():
            if tool.requires_approval or tool.tool_def.kind != "function" or tool.tool_def.defer_loading:
                raise ValueError("Pydantic AI native deferred tools and approvals are unsupported")
            if inspect.isgeneratorfunction(tool.function) or inspect.isasyncgenfunction(tool.function):
                raise ValueError("Pydantic AI streaming tool results are unsupported")

    def check_capability(capability):
        module = type(capability).__module__
        if (
            capability.defer_loading
            or module.startswith("pydantic_ai.durable_exec")
            or module
            in {
                "pydantic_ai.capabilities.instrumentation",
                "pydantic_ai.capabilities.caching",
            }
        ):
            raise ValueError("Pydantic AI deferred, durable, cached and native tracing capabilities are unsupported")

    agent.root_capability.apply(check_capability)


def _json_output(value: Any) -> Any:
    """Use native Pydantic serializers, then require finite, bounded JSON."""
    from pydantic import BaseModel
    from pydantic_core import to_jsonable_python

    def check(item: Any, parents: set[int]) -> None:
        if type(item).__module__.startswith("pydantic_ai.") and not isinstance(item, (BaseModel, Enum)):
            raise ValueError("Pydantic AI rich or multimodal native results are unsupported; return JSON values")
        if isinstance(item, (bytes, bytearray, memoryview, AsyncIterable, Iterator)) or inspect.isawaitable(item):
            raise ValueError("Pydantic AI results must be JSON, without binary or streaming values")
        if isinstance(item, (dict, list, tuple, BaseModel, Enum)) or is_dataclass(item):
            if id(item) in parents:
                raise ValueError("Pydantic AI results contain a cycle")
            parents.add(id(item))
            try:
                if isinstance(item, BaseModel):
                    items = [getattr(item, name) for name in type(item).model_fields]
                    items.extend((item.model_extra or {}).values())
                elif isinstance(item, Enum):
                    items = [item.value]
                elif is_dataclass(item):
                    items = [getattr(item, field.name) for field in fields(item)]
                elif isinstance(item, dict):
                    if any(not isinstance(key, str) for key in item):
                        raise ValueError("Pydantic AI JSON object keys must be strings")
                    items = item.values()
                else:
                    items = item
                for child in items:
                    check(child, parents)
            finally:
                parents.remove(id(item))

    check(value, set())
    result = to_jsonable_python(value, round_trip=True, by_alias=True, serialize_unknown=False)
    _bounded(result)
    return result


class PydanticAIAdapter:
    checkpoint_framework = ""

    def __init__(self) -> None:
        self.factories: dict[str, AgentFactory] = {}
        self._http_address = "http://localhost:2026"
        self._runner_kind = "python-pydantic-ai"
        self._runner_token: str | None = None

    def configure(
        self, *, http_address: str, runner_kind: str = "python-pydantic-ai", runner_token: str | None = None
    ) -> None:
        self._http_address, self._runner_kind, self._runner_token = http_address, runner_kind, runner_token

    def _state(self) -> PydanticAIState:
        base = resolve_checkpoint_http_url(self._http_address)
        if not base:
            raise CompletionDeliveryError("Pydantic AI managed runs require RUNKITE_HTTP_URL")
        return PydanticAIState(
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
                f"runkite_pydantic_ai.{graph_id}", (path.parent / file).resolve()
            )
            if spec is None or spec.loader is None:
                raise ValueError("Cannot load Pydantic AI factory module")
            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)
            factory = getattr(module, export)
            if not callable(factory):
                raise ValueError("Pydantic AI exports must be per-run factories")
            self.factories[graph_id] = factory

    async def execute(self, assignment: dict, event_callback: EventCallback, cancel_event: asyncio.Event) -> str:
        from pydantic import TypeAdapter
        from pydantic_ai import AgentRunResult, AgentRunResultEvent, Conversation, ConversationTypeAdapter
        from pydantic_ai.capabilities import AbstractCapability
        from pydantic_ai.exceptions import RunCancelled as NativeRunCancelled
        from pydantic_ai.messages import (
            ModelRequest,
            ModelResponse,
            PartDeltaEvent,
            PartStartEvent,
            SystemPromptPart,
            TextPart,
            TextPartDelta,
            UserPromptPart,
        )

        run_id, graph_id = assignment["run_id"], assignment["graph_id"]
        thread_id, generation = assignment.get("thread_id") or "", int(assignment.get("generation") or 0)
        make_event = make_event_factory(run_id)

        async def error(exc):
            message = str(exc).encode("utf-8", errors="replace")[:65536].decode("utf-8", errors="ignore")
            await event_callback(make_event("error", {"message": message, "type": type(exc).__name__}))
            await event_callback(make_event("end", {"status": "error"}))

        try:
            if not thread_id:
                raise ValueError("thread_id is required")
            if assignment.get("checkpoint_ref") or assignment.get("resume_command") is not None:
                raise ValueError("Pydantic AI native resume and checkpoint_ref are unsupported")
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

        store, agent, entered, stream, watcher = self._state(), None, False, None, None
        usage, published_usage = None, None
        baseline = (0, 0, 0)
        models: list[str | None] = []
        started = False

        def check_cancel():
            if cancel_event.is_set():
                raise RunCancelled()

        def capture_usage(native, responses=None):
            nonlocal usage, models
            values = (native.input_tokens, native.output_tokens, native.requests)
            if any(type(value) is not int or value < before for value, before in zip(values, baseline, strict=True)):
                raise ValueError("Pydantic AI current-run usage cannot be negative or non-integral")
            prompt, completion, requests = [value - before for value, before in zip(values, baseline, strict=True)]
            usage = {"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion}
            if not prompt and not completion:
                usage = usage_or_unmetered(None, started)
            if responses is not None:
                responses = [
                    response
                    for response in responses
                    if response.state == "complete"
                    or response.usage.input_tokens
                    or response.usage.output_tokens
                    or any(
                        attempt.usage is not None and (attempt.usage.input_tokens or attempt.usage.output_tokens)
                        for attempt in response.failed_attempts or []
                    )
                ]
                history_models = [
                    response.model_name
                    if isinstance(response.model_name, str)
                    and response.model_name.strip()
                    and not response.failed_attempts
                    else None
                    for response in responses
                ]
                # A cancellation inside the awaited usage callback can occur
                # before the SDK appends that observed response to history.
                if len(history_models) >= len(models):
                    models = history_models
            identities = set(models)
            if usage and len(identities) == 1 and None not in identities and requests <= len(models):
                usage["model"] = models[0]

        def partial_values():
            return values_with_usage(
                {"pydantic_ai": {"framework": FRAMEWORK, "generation": generation}}, usage_or_unmetered(usage, started)
            )

        async def publish_usage():
            nonlocal published_usage
            current = partial_values().get("usage")
            if current is not None and current != published_usage:
                values = partial_values()
                _bounded(values)
                await event_callback(make_event("values", values))
                published_usage = dict(current)

        class ManagedCapability(AbstractCapability):
            id = "runkite-managed"

            async def before_model_request(self, ctx, request_context):
                nonlocal started
                check_cancel()
                _supported_model(request_context.model)
                _, params = request_context.model.prepare_request(
                    request_context.model_settings, request_context.model_request_parameters
                )
                if params.native_tools or params.output_mode not in ("text", "tool"):
                    raise ValueError("managed Pydantic AI supports text/default tool-schema mode without native tools")
                if any(tool.kind != "function" or tool.defer_loading for tool in params.function_tools):
                    raise ValueError("Pydantic AI deferred tools and approvals are unsupported")
                started = True
                return request_context

            async def after_model_request(self, ctx, *, request_context, response):
                model = response.model_name or getattr(request_context.model, "model_name", None)
                models.append(
                    model if isinstance(model, str) and model.strip() and not response.failed_attempts else None
                )
                capture_usage(ctx.usage)
                await publish_usage()
                check_cancel()
                return response

            async def before_tool_execute(self, ctx, *, call, tool_def, args):
                check_cancel()
                if tool_def.kind != "function":
                    raise ValueError("managed Pydantic AI supports local function execution only")
                capture_usage(ctx.usage)
                await publish_usage()
                payload = {"id": call.tool_call_id, "name": tool_def.name, "args": call.args_as_dict()}
                _bounded(payload)
                await event_callback(make_event("tool_call", payload))
                if allowed is not None and tool_def.name not in allowed:
                    await event_callback(
                        make_event(
                            "tool_auth",
                            {
                                "stage": "tool.call",
                                "effect": "deny",
                                "tool": tool_def.name,
                                "reason": "tool is not permitted by allowed_tools",
                                "reason_code": "tool_not_allowed",
                                "generation": generation,
                            },
                        )
                    )
                    raise ValueError(f"Tool {tool_def.name!r} is not permitted by allowed_tools")
                check_cancel()
                return args

            async def after_tool_execute(self, ctx, *, call, tool_def, args, result):
                if isinstance(result, (AsyncIterable, Iterator)) or inspect.isawaitable(result):
                    if hasattr(result, "aclose"):
                        await result.aclose()
                    elif hasattr(result, "close"):
                        result.close()
                    raise ValueError("Pydantic AI streaming tool results are unsupported")
                _json_output(result)
                check_cancel()
                return result

        try:
            prior, etag = await store.load(thread_id, graph_id=graph_id)
            check_cancel()
            await event_callback(make_event("lifecycle", {"event": "running"}))
            completed = (prior or {}).get("completed")
            if completed and completed["run_id"] == run_id:
                if completed["input"] != input_key:
                    raise ValueError("reclaimed run input differs from its committed turn")
                output = json.loads(state_json(completed["output"]))
                output["pydantic_ai"]["generation"] = generation
                _bounded(output)
                if len(state_json(completed["text_events"]).encode()) > MAX_RECEIPT_BYTES:
                    raise ValueError("Pydantic AI text receipt exceeds the 8 MiB limit")
                for delta in completed["text_events"]:
                    _message_payload(run_id, delta)
                for delta in completed["text_events"]:
                    check_cancel()
                    if delta:
                        await event_callback(make_event("messages", _message_payload(run_id, delta)))
                await event_callback(make_event("values", output))
                await event_callback(make_event("end", {"status": "success"}))
                return "success"

            context = PydanticAIRunContext(
                run_id, thread_id, assignment.get("tenant_id") or "default", generation, graph_id, cancel_event
            )

            async def build():
                check_cancel()
                result = factory(context)
                return await result if inspect.isawaitable(result) else result

            definition = await run_cancellable(build(), cancel_event)
            agent = definition.agent if isinstance(definition, PydanticAIAgentDefinition) else definition
            deps = definition.deps if isinstance(definition, PydanticAIAgentDefinition) else None
            _supported_agent(agent)
            agent.instrument = False
            conversation = (
                ConversationTypeAdapter.validate_json(state_json(prior["history"])) if prior else Conversation()
            )
            baseline = (conversation.usage.input_tokens, conversation.usage.output_tokens, conversation.usage.requests)
            conversation.messages.append(
                ModelRequest(
                    parts=[
                        SystemPromptPart(message["content"])
                        if message["role"] == "system"
                        else UserPromptPart(message["content"])
                        for message in incoming
                    ]
                )
            )
            if len(state_json(dump_conversation(conversation)).encode()) > MAX_STATE_BYTES:
                raise ValueError("Pydantic AI conversation exceeds the 16 MiB checkpoint limit")
            check_cancel()
            await run_cancellable(agent.__aenter__(), cancel_event)
            entered = True
            text_events, assistant_text, receipt_bytes, final = [], "", 2, None
            async with agent.run_stream_events(
                conversation=conversation,
                run_id=run_id,
                deps=deps,
                capabilities=[ManagedCapability()],
            ) as events:
                stream = events

                async def watch_cancel():
                    await cancel_event.wait()
                    events.cancel()

                watcher = asyncio.create_task(watch_cancel())
                async for event in events:
                    check_cancel()
                    if isinstance(event, AgentRunResultEvent):
                        if final is not None:
                            raise ValueError("Pydantic AI emitted more than one completed result")
                        final = event.result
                    else:
                        delta = (
                            event.part.content
                            if isinstance(event, PartStartEvent) and isinstance(event.part, TextPart)
                            else (
                                event.delta.content_delta
                                if isinstance(event, PartDeltaEvent) and isinstance(event.delta, TextPartDelta)
                                else ""
                            )
                        )
                        if delta:
                            payload = _message_payload(run_id, delta)
                            receipt_bytes += len(state_json(delta).encode()) + bool(text_events)
                            if receipt_bytes > MAX_RECEIPT_BYTES:
                                raise ValueError("Pydantic AI text receipt exceeds the 8 MiB limit")
                            text_events.append(delta)
                            assistant_text += delta
                            await event_callback(make_event("messages", payload))
            check_cancel()
            if final is None or final.conversation.deferred_tool_requests is not None:
                raise ValueError("Pydantic AI did not complete a supported native result")
            responses = [message for message in final.new_messages() if isinstance(message, ModelResponse)]
            if not responses:
                raise ValueError("Pydantic AI managed runs require a completed model response")
            native_text = "".join(
                part.content for message in responses for part in message.parts if isinstance(part, TextPart)
            )
            if native_text != assistant_text:
                raise ValueError("Pydantic AI streamed text differs from completed native messages")
            capture_usage(final.usage, responses)
            await publish_usage()
            messages = [*((prior or {}).get("messages") or []), *incoming]
            if assistant_text:
                messages.append({"role": "assistant", "content": assistant_text})
            output = values_with_usage(
                {"messages": messages, "pydantic_ai": {"framework": FRAMEWORK, "generation": generation}},
                usage_or_unmetered(usage, started),
            )
            result_metadata = TypeAdapter(AgentRunResult[Any]).dump_python(
                final, mode="json", include={"output_tool_name"}
            )
            if result_metadata.get("output_tool_name"):
                output["result"] = _json_output(final.output)
            elif not isinstance(final.output, str):
                raise ValueError("Pydantic AI text result must be a string")
            _bounded(output)
            envelope = {
                "v": 1,
                "framework": FRAMEWORK,
                "graph_id": graph_id,
                "history": dump_conversation(final.conversation),
                "messages": messages,
                "completed": {
                    "status": "success",
                    "run_id": run_id,
                    "input": input_key,
                    "output": output,
                    "text_events": text_events,
                },
            }
            encode_state(envelope, graph_id=graph_id)
            await store.save(thread_id, envelope, etag, graph_id=graph_id)
            await event_callback(make_event("values", output))
            await event_callback(make_event("end", {"status": "success"}))
            return "success"
        except (RunCancelled, NativeRunCancelled):
            if stream is not None:
                with contextlib.suppress(Exception):
                    capture_usage(stream.usage, [m for m in stream.new_messages() if isinstance(m, ModelResponse)])
            await event_callback(make_event("values", partial_values()))
            await event_callback(make_event("end", {"status": "interrupted"}))
            return "interrupted"
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            if stream is not None:
                with contextlib.suppress(Exception):
                    capture_usage(stream.usage, [m for m in stream.new_messages() if isinstance(m, ModelResponse)])
            if started or usage:
                await event_callback(make_event("values", partial_values()))
            if cancel_event.is_set():
                await event_callback(make_event("end", {"status": "interrupted"}))
                return "interrupted"
            logger.exception("Pydantic AI run failed (%s)", type(exc).__name__)
            await error(exc)
            return "error"
        finally:
            if watcher is not None:
                watcher.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await watcher
            if entered:
                try:
                    await agent.__aexit__(None, None, None)
                except Exception as exc:
                    logger.warning("Pydantic AI agent close failed (%s)", type(exc).__name__)
            try:
                await store.aclose()
            except Exception as exc:
                logger.warning("Pydantic AI state close failed (%s)", type(exc).__name__)
