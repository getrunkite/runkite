"""Managed OpenAI Agents SDK text turns over the Runkite Runner Protocol."""

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
from runkite_runner.usage import usage_or_unmetered, usage_payload, values_with_usage

from .state import FRAMEWORK, MAX_STATE_BYTES, OpenAIAgentsState, encode_state, state_json

logger = logging.getLogger("runkite.runner.openai_agents")
AgentFactory = Callable[["OpenAIAgentsRunContext"], Any | Awaitable[Any]]
MAX_OUTPUT_BYTES = 3 << 20
MAX_RECEIPT_BYTES = 8 << 20
MODEL_METADATA_WAIT_SECONDS = 1.0


@dataclass(frozen=True)
class OpenAIAgentsRunContext:
    run_id: str
    thread_id: str
    tenant_id: str
    generation: int
    graph_id: str
    cancel_event: asyncio.Event


def _user_messages(input_data: Any) -> list[dict[str, str]]:
    if not isinstance(input_data, dict) or set(input_data) != {"messages"}:
        raise ValueError("managed OpenAI Agents input must contain only messages; native state/resume is unsupported")
    raw = input_data["messages"]
    if not isinstance(raw, list) or not raw:
        raise ValueError("input.messages is required")
    messages: list[dict[str, str]] = []
    for item in raw:
        if not isinstance(item, dict):
            raise ValueError("each input message must be an object")
        role = item.get("role") or item.get("type")
        if role not in ("user", "human", "system"):
            raise ValueError("managed OpenAI Agents turns accept new user/system text messages only")
        content = item.get("content")
        if isinstance(content, list):
            if any(
                not isinstance(part, dict) or part.get("type") != "text" or not isinstance(part.get("text"), str)
                for part in content
            ):
                raise ValueError("managed OpenAI Agents messages support text parts only")
            content = "".join(part["text"] for part in content)
        if not isinstance(content, str):
            raise ValueError("message content must be text")
        messages.append({"role": "user" if role == "human" else role, "content": content})
    if messages[-1]["role"] != "user" or not messages[-1]["content"].strip():
        raise ValueError("the newest message must be a nonempty user turn")
    if len(state_json(messages).encode("utf-8")) > MAX_OUTPUT_BYTES:
        raise ValueError("OpenAI Agents input exceeds the 3 MiB output event limit")
    return messages


def _bounded_output(output: dict[str, Any]) -> None:
    if len(state_json(output).encode("utf-8")) > MAX_OUTPUT_BYTES:
        raise ValueError("OpenAI Agents output exceeds the 3 MiB event limit")


def _error_payload(error: Exception) -> dict[str, str]:
    message = str(error).encode("utf-8", errors="replace")
    if len(message) > 64 << 10:
        message = message[: 64 << 10].decode("utf-8", errors="ignore").encode() + b" [truncated]"
    return {"message": message.decode("utf-8"), "type": type(error).__name__}


def _completion_delivery_error(error: BaseException) -> CompletionDeliveryError | None:
    """SDK tool errors can wrap a callback's delivery failure in UserError."""
    seen: set[int] = set()
    while id(error) not in seen:
        if isinstance(error, CompletionDeliveryError):
            return error
        seen.add(id(error))
        error = error.__cause__ or error.__context__
        if error is None:
            break
    return None


def _message_payload(run_id: str, delta: str) -> list[Any]:
    payload = [{"id": f"{run_id}:assistant", "type": "ai", "content": delta}, {}]
    if len(state_json(payload).encode("utf-8")) > MAX_OUTPUT_BYTES:
        raise ValueError("OpenAI Agents message exceeds the 3 MiB event limit")
    return payload


def _native_usage(native: Any) -> dict[str, Any] | None:
    prompt = getattr(native, "input_tokens", 0)
    completion = getattr(native, "output_tokens", 0)
    total = getattr(native, "total_tokens", 0)
    if any(type(value) is not int or value < 0 for value in (prompt, completion, total)):
        raise ValueError("OpenAI Agents usage must contain nonnegative integer token counts")
    if not prompt and not completion and not total:
        return None
    return usage_payload({"prompt_tokens": prompt or max(total - completion, 0), "completion_tokens": completion})


def _model_name(value: Any) -> str | None:
    return value if isinstance(value, str) and value.strip() else None


def _require_supported_tool(tool: Any) -> None:
    from agents import FunctionTool
    from agents.tool import ToolOriginType, get_function_tool_origin

    if not isinstance(tool, FunctionTool):
        raise ValueError("managed OpenAI Agents supports ordinary local FunctionTools only")
    origin = get_function_tool_origin(tool)
    if origin is not None and origin.type != ToolOriginType.FUNCTION:
        raise ValueError("managed OpenAI Agents does not support MCP or Agent.as_tool tools")
    if tool.needs_approval is not False:
        raise ValueError("managed OpenAI Agents native tool approval/resume is unsupported")
    if tool.defer_loading or tool.allowed_callers is not None:
        raise ValueError("managed OpenAI Agents deferred and programmatic tools are unsupported")
    for function in (getattr(tool, "__wrapped__", None), tool.on_invoke_tool):
        if inspect.isgeneratorfunction(function) or inspect.isasyncgenfunction(function):
            raise ValueError("managed OpenAI Agents streaming tool outputs are unsupported")


def _require_supported_agent(agent: Any) -> None:
    from agents import Agent

    if not isinstance(agent, Agent):
        raise ValueError("OpenAI Agents factory must return an agents.Agent")
    if agent.handoffs or agent.mcp_servers:
        raise ValueError("managed OpenAI Agents handoffs and native MCP servers are unsupported")
    if agent.prompt is not None:
        raise ValueError("managed OpenAI Agents server prompt configurations are unsupported")
    if agent.output_type not in (None, str):
        raise ValueError("managed OpenAI Agents supports text output only")
    if agent.tool_use_behavior != "run_llm_again":
        raise ValueError("managed OpenAI Agents requires run_llm_again tool behavior")
    for tool in agent.tools:
        _require_supported_tool(tool)


def _native_text(result: Any) -> str:
    """Project public assistant message items, including ordinary text refusals."""
    text: list[str] = []
    last_text: str | None = None
    for item in result.new_items:
        if item.type != "message_output_item":
            continue
        message = item.raw_item
        if message.status != "completed":
            raise ValueError("OpenAI Agents returned an unfinished assistant message")
        parts: list[str] = []
        for part in message.content:
            if part.type == "output_text":
                parts.append(part.text)
            elif part.type == "refusal":
                parts.append(part.refusal)
            else:
                raise ValueError("OpenAI Agents returned unsupported assistant content")
        last_text = "".join(parts)
        text.append(last_text)
    if last_text is None or last_text != result.final_output:
        raise ValueError("OpenAI Agents final output does not match its completed native assistant message")
    return "".join(text)


class OpenAIAgentsAdapter:
    """Per-run Agent factories with successful-turn native history and CAS receipts."""

    checkpoint_framework = ""

    def __init__(self) -> None:
        self.factories: dict[str, AgentFactory] = {}
        self._http_address = "http://localhost:2026"
        self._runner_kind = "python-openai-agents"
        self._runner_token: str | None = None

    def configure(
        self,
        *,
        http_address: str,
        runner_kind: str = "python-openai-agents",
        runner_token: str | None = None,
    ) -> None:
        self._http_address = http_address
        self._runner_kind = runner_kind
        self._runner_token = runner_token

    def _state(self) -> OpenAIAgentsState:
        base = resolve_checkpoint_http_url(self._http_address)
        if not base:
            raise CompletionDeliveryError("OpenAI Agents managed runs require RUNKITE_HTTP_URL")
        return OpenAIAgentsState(
            OpaqueCheckpointClient(
                http_base_url=base,
                framework=FRAMEWORK,
                runner_kind=self._runner_kind,
                runner_token=self._runner_token,
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
            file_path, export_name = ref.split(":", 1)
            location = (path.parent / file_path).resolve()
            spec = importlib.util.spec_from_file_location(f"runkite_openai_agents.{graph_id}", location)
            if spec is None or spec.loader is None:
                raise ValueError(f"Cannot load OpenAI Agents module: {location}")
            module = importlib.util.module_from_spec(spec)
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)
            factory = getattr(module, export_name)
            if not callable(factory):
                raise ValueError(f"OpenAI Agents export {export_name!r} must be a factory callable")
            self.factories[graph_id] = factory

    async def execute(self, assignment: dict, event_callback: EventCallback, cancel_event: asyncio.Event) -> str:
        from agents import Agent, Model, RunConfig, RunHooks, Runner

        run_id, graph_id = assignment["run_id"], assignment["graph_id"]
        thread_id = assignment.get("thread_id") or ""
        generation = int(assignment.get("generation") or 0)
        tenant_id = assignment.get("tenant_id") or "default"
        make_event = make_event_factory(run_id)
        try:
            if not thread_id:
                raise ValueError("thread_id is required")
            if assignment.get("checkpoint_ref") or assignment.get("resume_command") is not None:
                raise ValueError("checkpoint_ref and native RunState/resume are unsupported for python-openai-agents")
            incoming = _user_messages(assignment.get("input"))
            input_key = state_json(incoming)
            allowed = assignment.get("allowed_tools")
            if allowed is not None and (
                not isinstance(allowed, list) or any(not isinstance(name, str) or not name for name in allowed)
            ):
                raise ValueError("allowed_tools must be a list of tool names")
            allowed_tools = set(allowed or [])
            factory = self.factories.get(graph_id)
            if factory is None:
                raise ValueError(f"unknown graph_id: {graph_id!r}")
        except ValueError as exc:
            await event_callback(make_event("error", _error_payload(exc)))
            await event_callback(make_event("end", {"status": "error"}))
            return "error"
        if cancel_event.is_set():
            await event_callback(make_event("end", {"status": "interrupted"}))
            return "interrupted"

        state_store = self._state()
        agent = None
        result = None
        events = None
        cancel_watcher = None
        usage: dict[str, Any] | None = None
        published_usage: dict[str, Any] | None = None
        model_metadata: dict[str, asyncio.Queue[str | None]] = {}
        completed_models: list[str | None] = []
        configured_model: str | None = None
        model_metadata_consistent = True
        started = False
        text_events: list[str] = []
        assistant_text = ""
        receipt_bytes = 2

        def check_cancel() -> None:
            if cancel_event.is_set():
                raise RunCancelled()

        def partial_values() -> dict[str, Any]:
            return values_with_usage(
                {"openai_agents": {"framework": FRAMEWORK, "generation": generation}},
                usage_or_unmetered(usage, started),
            )

        async def publish_usage(native: Any) -> None:
            nonlocal usage, published_usage
            usage = usage_or_unmetered(_native_usage(native) or usage, started)
            if usage is not None:
                usage = dict(usage)
                # One aggregate token total cannot be priced as a single model
                # if any observed call has an unknown or different identity.
                usage.pop("model", None)
                identities = set(completed_models)
                requests = getattr(native, "requests", 0)
                # Cancellation can settle the SDK before this attempt's last
                # end hook records its identity. Retries may also aggregate
                # more physical requests than identities exposed by the SDK.
                all_requests_identified = type(requests) is int and 0 <= requests <= len(completed_models)
                if len(identities) == 1 and None not in identities and all_requests_identified:
                    usage["model"] = completed_models[0]
            current = partial_values().get("usage")
            if current is not None and current != published_usage:
                values = partial_values()
                _bounded_output(values)
                await event_callback(make_event("values", values))
                published_usage = dict(current)

        async def emit_text(delta: str) -> None:
            nonlocal assistant_text, receipt_bytes
            payload = _message_payload(run_id, delta)
            receipt_bytes += len(state_json(delta).encode("utf-8")) + bool(text_events)
            if receipt_bytes > MAX_RECEIPT_BYTES:
                raise ValueError("OpenAI Agents text receipt exceeds the 8 MiB limit")
            assistant_text += delta
            text_events.append(delta)
            await event_callback(make_event("messages", payload))

        class ManagedHooks(RunHooks):
            async def on_llm_start(self, context, current_agent, system_prompt, input_items) -> None:
                nonlocal started, configured_model
                check_cancel()
                _require_supported_agent(current_agent)
                if len(state_json(input_items).encode("utf-8")) > MAX_STATE_BYTES:
                    raise ValueError("OpenAI Agents native input exceeds the 16 MiB checkpoint limit")
                configured_model = _model_name(current_agent.model) or _model_name(
                    getattr(current_agent.model, "model", None)
                )
                started = True

            async def on_llm_end(self, context, current_agent, response) -> None:
                nonlocal model_metadata_consistent
                # The SDK awaits this hook before executing tools or another
                # model step. Queue consumption alone is too late for cancel.
                # A completed raw response is queued before this hook. Wait
                # for its reported identity so pricing precedes tool effects.
                reported_model = None
                if model_metadata_consistent and isinstance(response.response_id, str):
                    try:
                        reported_model = await asyncio.wait_for(
                            model_metadata.setdefault(response.response_id, asyncio.Queue()).get(),
                            timeout=MODEL_METADATA_WAIT_SECONDS,
                        )
                    except TimeoutError:
                        # Do not pair a late event with another occurrence of
                        # a provider's repeated response ID. Keep this run
                        # unpriced if metadata delivery cannot be aligned.
                        model_metadata_consistent = False
                completed_models.append((reported_model or configured_model) if model_metadata_consistent else None)
                await publish_usage(context.usage)
                check_cancel()

            async def on_tool_start(self, context, current_agent, tool) -> None:
                check_cancel()
                _require_supported_tool(tool)
                arguments = json.loads(context.tool_arguments)
                call = {"id": context.tool_call_id, "name": tool.name, "args": arguments}
                _bounded_output(call)
                await event_callback(make_event("tool_call", call))
                if allowed is not None and tool.name not in allowed_tools:
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
                check_cancel()

            async def on_tool_end(self, context, current_agent, tool, output) -> None:
                if isinstance(output, (AsyncIterable, Iterator)) or inspect.isawaitable(output):
                    if hasattr(output, "aclose"):
                        await output.aclose()
                    elif hasattr(output, "close"):
                        output.close()
                    raise ValueError("managed OpenAI Agents streaming or awaitable tool outputs are unsupported")
                from agents import ItemHelpers

                native_output = ItemHelpers.tool_call_output_item(
                    context.tool_call, output, output_json_schema=tool.output_json_schema
                )
                chunks = native_output["output"]
                if not isinstance(chunks, str) and (
                    not isinstance(chunks, list)
                    or any(
                        chunk.get("type") != "input_text" or not isinstance(chunk.get("text"), str) for chunk in chunks
                    )
                ):
                    raise ValueError("managed OpenAI Agents tool results support text only")
                _bounded_output(native_output)
                check_cancel()

        try:
            prior, etag = await state_store.load(thread_id, graph_id=graph_id)
            check_cancel()
            await event_callback(make_event("lifecycle", {"event": "running"}))
            completed = (prior or {}).get("completed")
            if completed is not None and completed["run_id"] == run_id:
                if completed["input"] != input_key:
                    raise ValueError("reclaimed run input differs from its committed turn")
                output = json.loads(state_json(completed["output"]))
                output["openai_agents"]["generation"] = generation
                _bounded_output(output)
                saved_text = completed["text_events"]
                if len(state_json(saved_text).encode("utf-8")) > MAX_RECEIPT_BYTES:
                    raise ValueError("OpenAI Agents text receipt exceeds the 8 MiB limit")
                for delta in saved_text:
                    _message_payload(run_id, delta)
                for delta in saved_text:
                    check_cancel()
                    if delta:
                        await event_callback(make_event("messages", _message_payload(run_id, delta)))
                check_cancel()
                await event_callback(make_event("values", output))
                await event_callback(make_event("end", {"status": "success"}))
                return "success"

            context = OpenAIAgentsRunContext(
                run_id=run_id,
                thread_id=thread_id,
                tenant_id=tenant_id,
                generation=generation,
                graph_id=graph_id,
                cancel_event=cancel_event,
            )

            async def build_agent():
                check_cancel()
                built = factory(context)
                return await built if hasattr(built, "__await__") else built

            agent = await run_cancellable(build_agent(), cancel_event)
            _require_supported_agent(agent)
            check_cancel()
            native_input = [*((prior or {}).get("history") or []), *incoming]
            result = Runner.run_streamed(
                agent,
                native_input,
                context=context,
                hooks=ManagedHooks(),
                run_config=RunConfig(tracing_disabled=True),
            )

            async def watch_cancel() -> None:
                await cancel_event.wait()
                result.cancel()

            cancel_watcher = asyncio.create_task(watch_cancel())
            events = result.stream_events()
            async for event in events:
                check_cancel()
                if event.type == "raw_response_event":
                    kind = event.data.type
                    if kind == "response.completed":
                        response = event.data.response
                        if isinstance(response.id, str):
                            model_metadata.setdefault(response.id, asyncio.Queue()).put_nowait(
                                _model_name(getattr(response, "model", None))
                            )
                    if kind in ("response.output_text.delta", "response.refusal.delta") and event.data.delta:
                        await emit_text(event.data.delta)
                elif event.type == "agent_updated_stream_event":
                    if event.new_agent is not agent:
                        raise ValueError("managed OpenAI Agents handoffs are unsupported")
                elif event.name in (
                    "handoff_requested",
                    "handoff_occured",
                    "mcp_approval_requested",
                    "mcp_approval_response",
                ):
                    raise ValueError("managed OpenAI Agents handoffs and native approval/resume are unsupported")
            await publish_usage(result.context_wrapper.usage)
            check_cancel()
            if not result.is_complete or result.interruptions or not isinstance(result.final_output, str):
                raise ValueError("OpenAI Agents turn did not complete with a final text response")
            native_text = _native_text(result)
            if not assistant_text and native_text:
                await emit_text(native_text)
            elif assistant_text != native_text:
                raise ValueError("streamed text differs from completed OpenAI Agents messages")
            check_cancel()
            history = json.loads(state_json(result.to_input_list()))
            messages = [*((prior or {}).get("messages") or []), *incoming]
            if assistant_text:
                messages.append({"role": "assistant", "content": assistant_text})
            output = values_with_usage(
                {"messages": messages, "openai_agents": {"framework": FRAMEWORK, "generation": generation}},
                usage_or_unmetered(usage, started),
            )
            _bounded_output(output)
            envelope = {
                "v": 1,
                "framework": FRAMEWORK,
                "graph_id": graph_id,
                "history": history,
                "messages": messages,
                "completed": {
                    "run_id": run_id,
                    "input": input_key,
                    "status": "success",
                    "output": output,
                    "text_events": text_events,
                },
            }
            if len(state_json(envelope).encode("utf-8")) > MAX_STATE_BYTES:
                raise ValueError("OpenAI Agents state exceeds the 16 MiB checkpoint limit")
            encode_state(envelope, graph_id=graph_id)
            await state_store.save(thread_id, envelope, etag, graph_id=graph_id)
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
            delivery_error = _completion_delivery_error(exc)
            if delivery_error is not None:
                raise CompletionDeliveryError("OpenAI Agents completion delivery was not confirmed") from exc
            if cancel_event.is_set():
                await event_callback(make_event("values", partial_values()))
                await event_callback(make_event("end", {"status": "interrupted"}))
                return "interrupted"
            logger.exception("OpenAI Agents run failed: %s", exc)
            if started or usage:
                await event_callback(make_event("values", partial_values()))
            await event_callback(make_event("error", _error_payload(exc)))
            await event_callback(make_event("end", {"status": "error"}))
            return "error"
        finally:
            if cancel_watcher is not None:
                cancel_watcher.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await cancel_watcher
            if result is not None:
                if not result.is_complete:
                    result.cancel()
                if events is not None:
                    with contextlib.suppress(Exception, asyncio.CancelledError):
                        await events.aclose()
                if result.run_loop_task is not None:
                    with contextlib.suppress(Exception, asyncio.CancelledError):
                        await result.run_loop_task
                result.release_agents()
            if isinstance(agent, Agent) and isinstance(agent.model, Model):
                try:
                    await agent.model.close()
                except Exception as exc:
                    logger.warning("OpenAI Agents model close failed (%s)", type(exc).__name__)
            try:
                await state_store.aclose()
            except Exception as exc:
                logger.warning("OpenAI Agents state client close failed (%s)", type(exc).__name__)
