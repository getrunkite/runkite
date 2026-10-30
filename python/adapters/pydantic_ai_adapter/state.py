"""Fenced native Pydantic AI conversations and committed successful receipts."""

from __future__ import annotations

import dataclasses
import json
import math
from decimal import Decimal
from typing import Any

from runkite_runner.completion import CompletionDeliveryError
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient, _strong_numeric_etag

FRAMEWORK = "pydantic-ai"
MAX_OUTPUT_BYTES = 3 << 20
MAX_RECEIPT_BYTES = 8 << 20
MAX_STATE_BYTES = 16 << 20
MAX_INT64 = (1 << 63) - 1


def state_json(value: Any) -> str:
    """Encode strict finite JSON without converting keys or invalid Unicode."""
    ancestors: set[int] = set()

    def validate(item: Any) -> None:
        if item is None or type(item) in (str, bool, int, float):
            return
        if type(item) not in (dict, list) or id(item) in ancestors:
            raise ValueError("state contains a non-JSON value or cycle")
        ancestors.add(id(item))
        try:
            if type(item) is dict:
                for key, child in item.items():
                    if type(key) is not str:
                        raise ValueError("state object keys must be strings")
                    validate(child)
            else:
                for child in item:
                    validate(child)
        finally:
            ancestors.remove(id(item))

    validate(value)
    encoded = json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    encoded.encode("utf-8")
    return encoded


def _canonical(value: Any) -> str:
    state_json(value)
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False)


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("state contains duplicate object keys")
        value[key] = item
    return value


def _keys(value: Any, required: set[str], optional: set[str] | None = None) -> None:
    if type(value) is not dict or not required <= value.keys() or value.keys() - required - (optional or set()):
        raise ValueError("state object has missing or unsupported fields")


def _finite_native(value: Any, ancestors: set[int] | None = None) -> None:
    """Prevent native serializers turning a nonfinite value into JSON null."""
    if isinstance(value, float) and not math.isfinite(value):
        raise ValueError("native conversation contains a nonfinite number")
    if isinstance(value, Decimal) and not value.is_finite():
        raise ValueError("native conversation contains a nonfinite decimal")
    if isinstance(value, str):
        value.encode("utf-8")
    ancestors = ancestors if ancestors is not None else set()
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        children = [getattr(value, field.name) for field in dataclasses.fields(value)]
    elif isinstance(value, dict):
        if any(type(key) is not str for key in value):
            raise ValueError("native JSON object keys must be strings")
        children = list(value.values())
    elif isinstance(value, (list, tuple)):
        children = value
    else:
        return
    if id(value) in ancestors:
        raise ValueError("native conversation contains a cycle")
    ancestors.add(id(value))
    try:
        for child in children:
            _finite_native(child, ancestors)
    finally:
        ancestors.remove(id(value))


def dump_conversation(conversation: Any) -> dict[str, Any]:
    """Canonicalize the public native codec, retaining its usage and identity."""
    from pydantic_ai.conversation import Conversation, ConversationTypeAdapter

    if not isinstance(conversation, Conversation):
        raise ValueError("native state requires a Pydantic AI Conversation")
    _finite_native(conversation)
    encoded = ConversationTypeAdapter.dump_json(conversation, warnings="error")
    # The public Usage codec emits Decimal cost as a string; strict=True
    # rejects that native JSON. Lossless canonical comparison and explicit
    # counter checks below provide strict state validation without losing it.
    restored = ConversationTypeAdapter.validate_json(encoded)
    value = json.loads(ConversationTypeAdapter.dump_json(restored, warnings="error"), object_pairs_hook=_unique_object)
    state_json(value)
    return value


def _messages(value: Any, *, incoming: bool = False) -> list[dict[str, str]]:
    if type(value) is not list or not value:
        raise ValueError("state requires text messages")
    for item in value:
        _keys(item, {"role", "content"})
        if (
            item["role"] not in ({"system", "user"} if incoming else {"system", "user", "assistant"})
            or type(item["content"]) is not str
        ):
            raise ValueError("state contains an invalid text message")
    if incoming and (value[-1]["role"] != "user" or not value[-1]["content"].strip()):
        raise ValueError("receipt input must end in a nonempty user turn")
    return value


def _native_usage(usage: Any) -> None:
    for field in dataclasses.fields(usage):
        name, value = field.name, getattr(usage, field.name)
        if name.endswith("tokens") or name in ("requests", "tool_calls", "unmeasured_requests"):
            if type(value) is not int or not 0 <= value <= MAX_INT64:
                raise ValueError("native usage counters must be nonnegative int64 values")
        elif name == "audio_seconds" and (type(value) not in (int, float) or not math.isfinite(value) or value < 0):
            raise ValueError("native usage duration is invalid")
        elif (
            name == "cost"
            and value is not None
            and (not isinstance(value, Decimal) or not value.is_finite() or value < 0)
        ):
            raise ValueError("native usage cost is invalid")
        elif name == "details" and (
            type(value) is not dict or any(type(count) is not int or count < 0 for count in value.values())
        ):
            raise ValueError("native usage details must be nonnegative counters")


def _conversation(value: Any) -> tuple[list[dict[str, str]], list[dict[str, str]], Any, list[Any], bool]:
    from pydantic_ai.conversation import ConversationTypeAdapter
    from pydantic_ai.messages import (
        ModelRequest,
        ModelResponse,
        RetryFeedbackPart,
        SystemPromptPart,
        TextPart,
        ThinkingPart,
        ToolCallPart,
        ToolReturnPart,
        UserPromptPart,
    )

    state_json(value)
    native = ConversationTypeAdapter.validate_json(state_json(value))
    if _canonical(dump_conversation(native)) != _canonical(value):
        raise ValueError("native conversation is not canonical or would lose data")
    if not native.conversation_id or native.deferred_tool_requests is not None or not native.messages:
        raise ValueError("native conversation is empty, unidentified or waiting for deferred tools")
    _native_usage(native.usage)
    pending: dict[str, str] = {}
    visible: list[dict[str, str]] = []
    inputs: list[dict[str, str]] = []
    responses: list[Any] = []
    output_ack = False
    has_user = False
    active_run: str | None = None
    for message in native.messages:
        if message.state != "complete" or message.conversation_id not in (None, native.conversation_id):
            raise ValueError("native message is partial or belongs to another conversation")
        if isinstance(message, ModelResponse):
            if not has_user or pending:
                raise ValueError("native model response precedes input or unresolved tool results")
            if not isinstance(message.run_id, str) or not message.run_id:
                raise ValueError("native model responses require a run identity")
            if active_run is not None and active_run != message.run_id:
                raise ValueError("native run identity changed within a user turn")
            active_run = message.run_id
            _native_usage(message.usage)
            for attempt in message.failed_attempts or []:
                if attempt.usage is not None:
                    _native_usage(attempt.usage)
            responses.append(message)
            output_ack = False
        for part in message.parts:
            if isinstance(message, ModelRequest) and isinstance(part, (UserPromptPart, SystemPromptPart)):
                if type(part.content) is not str:
                    raise ValueError("native input supports text only")
                role = "user" if isinstance(part, UserPromptPart) else "system"
                item = {"role": role, "content": part.content}
                inputs.append(item)
                if role == "user":
                    if pending:
                        raise ValueError("native user input interrupts unresolved tool calls")
                    has_user = True
                    output_ack = False
                    active_run = None
                    visible.append(item)
            elif isinstance(message, ModelResponse) and isinstance(part, TextPart):
                if part.content:
                    if visible and visible[-1]["role"] == "assistant":
                        visible[-1]["content"] += part.content
                    else:
                        visible.append({"role": "assistant", "content": part.content})
            elif isinstance(message, ModelResponse) and isinstance(part, ThinkingPart):
                continue
            elif isinstance(message, ModelResponse) and type(part) is ToolCallPart:
                if (
                    not part.tool_name
                    or not part.tool_call_id
                    or part.tool_kind is not None
                    or part.tool_call_id in pending
                ):
                    raise ValueError("native tool calls require unambiguous ordinary identities")
                pending[part.tool_call_id] = part.tool_name
            elif isinstance(message, ModelRequest) and type(part) is ToolReturnPart:
                if (
                    pending.get(part.tool_call_id) != part.tool_name
                    or part.tool_kind is not None
                    or part.outcome == "interrupted"
                ):
                    raise ValueError("native tool return is unmatched, interrupted or unsupported")
                del pending[part.tool_call_id]
                if (
                    (part.tool_name == "final_result" or part.tool_name.startswith("final_result_"))
                    and part.content == "Final result processed."
                    and part.outcome == "success"
                ):
                    output_ack = True
            elif isinstance(message, ModelRequest) and isinstance(part, RetryFeedbackPart):
                continue
            else:
                raise ValueError("native conversation contains an unsupported message part")
    if pending or not responses:
        raise ValueError("native conversation contains unresolved tool calls")
    if not output_ack and (
        not isinstance(native.messages[-1], ModelResponse)
        or not any(isinstance(part, TextPart) for part in native.messages[-1].parts)
    ):
        raise ValueError("native conversation lacks a completed text or typed result")
    counted = [item.usage for item in responses] + [
        attempt.usage for item in responses for attempt in item.failed_attempts or [] if attempt.usage is not None
    ]
    if native.usage.input_tokens < sum(item.input_tokens for item in counted) or native.usage.output_tokens < sum(
        item.output_tokens for item in counted
    ):
        raise ValueError("native conversation lost its cumulative usage")
    return visible, inputs, native, responses, output_ack


def _usage(value: Any) -> None:
    _keys(value, {"prompt_tokens", "completion_tokens", "total_tokens"}, {"model", "unmetered"})
    if any(
        type(value[key]) is not int or not 0 <= value[key] <= MAX_INT64
        for key in ("prompt_tokens", "completion_tokens", "total_tokens")
    ):
        raise ValueError("receipt usage must contain nonnegative int64 counters")
    if value["total_tokens"] != value["prompt_tokens"] + value["completion_tokens"]:
        raise ValueError("receipt token counts do not match")
    if "model" in value and (type(value["model"]) is not str or not value["model"].strip()):
        raise ValueError("receipt model must be a nonempty string")
    if "unmetered" in value and type(value["unmetered"]) is not bool:
        raise ValueError("receipt unmetered flag must be boolean")


def _validate_envelope(value: Any, graph_id: str) -> None:
    if type(graph_id) is not str or not graph_id:
        raise ValueError("a trusted graph_id is required")
    state_json(value)
    _keys(value, {"v", "framework", "graph_id", "history", "messages", "completed"})
    if (
        type(value["v"]) is not int
        or value["v"] != 1
        or value["framework"] != FRAMEWORK
        or value["graph_id"] != graph_id
    ):
        raise ValueError("native state version, framework or graph does not match")
    messages = _messages(value["messages"])
    visible, native_inputs, native, responses, output_ack = _conversation(value["history"])
    if _canonical(visible) != _canonical([item for item in messages if item["role"] != "system"]):
        raise ValueError("native history does not match its visible conversation")
    expected = [item for item in messages if item["role"] != "assistant"]
    index = 0
    for item in native_inputs:
        if index < len(expected) and item == expected[index]:
            index += 1
        elif item["role"] != "system":
            raise ValueError("native caller inputs do not match their conversation")
    if index != len(expected):
        raise ValueError("native caller system input is missing")
    receipt = value["completed"]
    _keys(receipt, {"status", "run_id", "input", "output", "text_events"})
    if (
        receipt["status"] != "success"
        or type(receipt["run_id"]) is not str
        or not receipt["run_id"]
        or type(receipt["input"]) is not str
    ):
        raise ValueError("native state requires a successful committed receipt")
    if responses[-1].run_id != receipt["run_id"]:
        raise ValueError("native latest run does not match its receipt")
    incoming = _messages(json.loads(receipt["input"], object_pairs_hook=_unique_object), incoming=True)
    if state_json(incoming) != receipt["input"]:
        raise ValueError("receipt input is not canonical JSON")
    text_events = receipt["text_events"]
    if type(text_events) is not list or any(type(delta) is not str for delta in text_events):
        raise ValueError("receipt text events must be strings")
    if len(state_json(text_events).encode()) > MAX_RECEIPT_BYTES:
        raise ValueError("Pydantic AI text receipt exceeds the 8 MiB limit")
    for delta in text_events:
        if (
            len(state_json([{"id": f"{receipt['run_id']}:assistant", "type": "ai", "content": delta}, {}]).encode())
            > MAX_OUTPUT_BYTES
        ):
            raise ValueError("Pydantic AI message exceeds the 3 MiB event limit")
    text = "".join(text_events)
    suffix = incoming + ([{"role": "assistant", "content": text}] if text else [])
    if len(messages) < len(suffix) or _canonical(messages[-len(suffix) :]) != _canonical(suffix):
        raise ValueError("receipt input/text does not match conversation history")
    output = receipt["output"]
    _keys(output, {"messages", "pydantic_ai", "usage"}, {"result"})
    _keys(output["pydantic_ai"], {"framework", "generation"})
    metadata = output["pydantic_ai"]
    if (
        _canonical(output["messages"]) != _canonical(messages)
        or metadata["framework"] != FRAMEWORK
        or type(metadata["generation"]) is not int
        or not 0 <= metadata["generation"] <= MAX_INT64
    ):
        raise ValueError("receipt output identity or history does not match")
    if ("result" in output) != output_ack:
        raise ValueError("receipt typed result does not match native output completion")
    if "usage" in output:
        _usage(output["usage"])
        latest_run = responses[-1].run_id
        current = [item for item in responses if item.run_id == latest_run]
        counted = [item.usage for item in current] + [
            attempt.usage for item in current for attempt in item.failed_attempts or [] if attempt.usage is not None
        ]
        prompt, completion = sum(item.input_tokens for item in counted), sum(item.output_tokens for item in counted)
        if output["usage"]["prompt_tokens"] != prompt or output["usage"]["completion_tokens"] != completion:
            raise ValueError("receipt usage must describe only its native run")
        if "model" in output["usage"] and any(
            item.model_name != output["usage"]["model"] or item.failed_attempts for item in current
        ):
            raise ValueError("receipt model must identify every current native response")
        if native.usage.input_tokens < prompt or native.usage.output_tokens < completion:
            raise ValueError("native conversation lost its cumulative usage")
    if len(state_json(output).encode()) > MAX_OUTPUT_BYTES:
        raise ValueError("Pydantic AI output exceeds the 3 MiB event limit")


def encode_state(value: Any, *, graph_id: str) -> bytes:
    """Validate locally produced state before classifying a commit as delivery."""
    body = state_json(value).encode("utf-8")
    if len(body) > MAX_STATE_BYTES:
        raise ValueError("Pydantic AI state exceeds the 16 MiB checkpoint limit")
    _validate_envelope(value, graph_id)
    return body


class PydanticAIState:
    def __init__(self, client: OpaqueCheckpointClient):
        self._client = client

    async def load(self, thread_id: str, *, graph_id: str) -> tuple[dict[str, Any] | None, str | None]:
        try:
            if type(graph_id) is not str or not graph_id:
                raise ValueError("a trusted graph_id is required")
            raw, etag = await self._client.get_versioned(thread_id)
            if raw is None:
                return None, None
            if not _strong_numeric_etag(etag) or len(raw) > MAX_STATE_BYTES:
                raise ValueError("native state needs a valid version and bounded checkpoint")
            value = json.loads(raw.decode("utf-8"), object_pairs_hook=_unique_object)
            _validate_envelope(value, graph_id)
            return value, etag
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            raise CompletionDeliveryError("Pydantic AI state could not be loaded safely") from exc

    async def save(self, thread_id: str, value: dict[str, Any], etag: str | None, *, graph_id: str) -> None:
        try:
            if etag is not None and not _strong_numeric_etag(etag):
                raise ValueError("native state requires a strong positive numeric ETag")
            body = encode_state(value, graph_id=graph_id)
            await self._client.put_cas(thread_id, body, etag=etag, create_only=etag is None)
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            raise CompletionDeliveryError("Pydantic AI state commit was not confirmed") from exc

    async def aclose(self) -> None:
        await self._client.aclose()
