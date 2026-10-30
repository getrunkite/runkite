"""Strict native Google ADK sessions and successful-turn CAS receipts."""

from __future__ import annotations

import json
import math
from typing import Any

from runkite_runner.completion import CompletionDeliveryError
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient, _strong_numeric_etag

FRAMEWORK = "google-adk"
MAX_OUTPUT_BYTES = 3 << 20
MAX_RECEIPT_BYTES = 8 << 20
MAX_STATE_BYTES = 16 << 20
MAX_INT64 = (1 << 63) - 1


def state_json(value: Any) -> str:
    """Encode finite JSON without coercing keys, objects or invalid Unicode."""
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
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("state contains duplicate object keys")
        result[key] = value
    return result


def _keys(value: Any, required: set[str], optional: set[str] | None = None) -> None:
    if type(value) is not dict or not required <= value.keys() or value.keys() - required - (optional or set()):
        raise ValueError("state object has missing or unsupported fields")


def validate_session_state(value: Any, *, durable: bool = False) -> None:
    """Reject cross-session scopes instead of making them thread-local."""
    if type(value) is not dict:
        raise ValueError("native session state must be a JSON object")
    state_json(value)
    prefixes = ("app:", "user:", "temp:") if durable else ("app:", "user:")
    if any(key.startswith(prefixes) for key in value):
        raise ValueError("shared app/user state and durable temp state are unsupported")


def _finite_native(value: Any, ancestors: set[int] | None = None) -> None:
    """Prevent native Pydantic serializers silently changing invalid numbers."""
    from pydantic import BaseModel

    if isinstance(value, float) and not math.isfinite(value):
        raise ValueError("native session contains a nonfinite number")
    if isinstance(value, str):
        value.encode("utf-8")
    if isinstance(value, BaseModel):
        children = [getattr(value, name) for name in type(value).model_fields]
    elif isinstance(value, dict):
        if any(type(key) is not str for key in value):
            raise ValueError("native object keys must be strings")
        children = list(value.values())
    elif isinstance(value, (tuple, list, set)):
        children = value
    else:
        return
    ancestors = ancestors if ancestors is not None else set()
    if id(value) in ancestors:
        raise ValueError("native session contains a cycle")
    ancestors.add(id(value))
    try:
        for child in children:
            _finite_native(child, ancestors)
    finally:
        ancestors.remove(id(value))


def dump_session(session: Any) -> dict[str, Any]:
    """Use the public native codec, including opaque thought signatures."""
    from google.adk.sessions import Session

    if not isinstance(session, Session):
        raise ValueError("native state requires a Google ADK Session")
    validate_session_state(session.state, durable=True)
    _finite_native(session)
    # state_delta has an SDK fallback serializer that stringifies unknown
    # objects. Reject those values before the serializer can lose their type.
    for event in session.events:
        validate_session_state(event.actions.state_delta, durable=True)
        state_json(event.custom_metadata)
        if event.content:
            for part in event.content.parts or []:
                state_json(part.part_metadata)
                if part.function_call is not None:
                    state_json(part.function_call.args)
                if part.function_response is not None:
                    state_json(part.function_response.response)
    encoded = session.model_dump_json(warnings="error")
    restored = Session.model_validate_json(encoded)
    value = json.loads(restored.model_dump_json(warnings="error"), object_pairs_hook=_unique_object)
    state_json(value)
    return value


def _identity(*, graph_id: str, app_name: str, user_id: str, session_id: str) -> None:
    if any(type(item) is not str or not item for item in (graph_id, app_name, user_id, session_id)):
        raise ValueError("trusted graph/app/user/session identity is required")
    if app_name != graph_id:
        raise ValueError("native app must match its managed graph")


def _counter(value: Any) -> int:
    if type(value) is not int or not 0 <= value <= MAX_INT64:
        raise ValueError("native usage counters must be nonnegative int64 values")
    return value


def native_usage(usage: Any) -> tuple[int, int]:
    """Map Gemini thoughts and tool-use prompt tokens into plane totals."""
    if usage is None:
        return 0, 0
    for name in type(usage).model_fields:
        value = getattr(usage, name)
        if name.endswith("_count") and value is not None:
            _counter(value)
        elif name.endswith("_details") and value is not None:
            for detail in value:
                if detail.token_count is not None:
                    _counter(detail.token_count)
    prompt = (usage.prompt_token_count or 0) + (usage.tool_use_prompt_token_count or 0)
    completion = (usage.candidates_token_count or 0) + (usage.thoughts_token_count or 0)
    _counter(prompt)
    _counter(completion)
    if usage.total_token_count is not None and usage.total_token_count != prompt + completion:
        raise ValueError("native usage total does not match its component counters")
    _counter(prompt + completion)
    return prompt, completion


def _messages(value: Any, *, incoming: bool = False) -> list[dict[str, str]]:
    if type(value) is not list or not value:
        raise ValueError("state requires text messages")
    for item in value:
        _keys(item, {"role", "content"})
        if item["role"] not in ({"user"} if incoming else {"user", "assistant"}) or type(item["content"]) is not str:
            raise ValueError("native profile requires user/assistant text messages")
    if incoming and (len(value) != 1 or not value[0]["content"].strip()):
        raise ValueError("receipt input requires exactly one nonempty user message")
    return value


def _session(
    value: Any, *, app_name: str, user_id: str, session_id: str
) -> tuple[Any, list[dict[str, str]], list[Any]]:
    from google.adk.events.event_actions import EventActions
    from google.adk.sessions import Session
    from google.genai import types

    native = Session.model_validate_json(state_json(value))
    if _canonical(dump_session(native)) != _canonical(value):
        raise ValueError("native session is not canonical or would lose data")
    if (native.app_name, native.user_id, native.id) != (app_name, user_id, session_id):
        raise ValueError("native session belongs to a different app, user or thread")
    if not native.events or not math.isfinite(native.last_update_time) or native.last_update_time < 0:
        raise ValueError("native session is empty or has an invalid update timestamp")
    defaults = EventActions().model_dump(mode="json")
    visible: list[dict[str, str]] = []
    model_events: list[Any] = []
    pending: dict[str, str] = {}
    current_invocation: str | None = None
    completed_invocations: set[str] = set()
    last_model_text = False
    for event in native.events:
        if (
            not event.id
            or not event.invocation_id
            or not event.author
            or not math.isfinite(event.timestamp)
            or event.timestamp < 0
            or event.partial
            or event.error_code
            or event.error_message
            or event.interrupted
            or event.long_running_tool_ids
            or event.branch is not None
            or event.isolation_scope is not None
            or event.output is not None
            or event.node_info.output_for is not None
            or event.node_info.message_as_output is not None
            or event.finish_reason not in (None, types.FinishReason.STOP)
        ):
            raise ValueError("native history contains an unfinished or unsupported event")
        if any(
            getattr(event, field) is not None
            for field in (
                "live_session_resumption_update",
                "interaction_status",
                "live_session_id",
                "go_away",
                "voice_activity",
                "input_transcription",
                "output_transcription",
                "interaction_id",
                "environment_id",
            )
        ):
            raise ValueError("native live/remote continuation is unsupported")
        actions = event.actions.model_dump(mode="json")
        if any(_canonical(item) != _canonical(defaults[key]) for key, item in actions.items() if key != "state_delta"):
            raise ValueError("native history contains unsupported control actions")
        validate_session_state(event.actions.state_delta, durable=True)
        if event.author == "user":
            if pending or (current_invocation is not None and not last_model_text):
                raise ValueError("native user input interrupts an unfinished invocation")
            if event.invocation_id in completed_invocations or event.invocation_id == current_invocation:
                raise ValueError("native invocation IDs cannot be reused for new user input")
            if current_invocation is not None:
                completed_invocations.add(current_invocation)
            current_invocation = event.invocation_id
            last_model_text = False
        elif current_invocation is None or event.invocation_id != current_invocation:
            raise ValueError("native event belongs to a different invocation")
        if event.content is None or not event.content.parts:
            raise ValueError("native history requires ordinary text or function content")
        text = ""
        has_call, has_response, has_text = False, False, False
        ordinary_text = False
        pending_before = bool(pending)
        for part in event.content.parts:
            allowed = {"text", "thought", "thought_signature", "function_call", "function_response", "part_metadata"}
            if any(getattr(part, name) is not None for name in type(part).model_fields if name not in allowed):
                raise ValueError("native content contains unsupported media or hosted tools")
            kinds = sum(item is not None for item in (part.text, part.function_call, part.function_response))
            if kinds > 1 or (kinds == 0 and part.thought_signature is None):
                raise ValueError("native content part is ambiguous or empty")
            if part.text is not None:
                has_text = True
                if not part.thought:
                    ordinary_text = True
                    text += part.text
            if part.function_call is not None:
                call = part.function_call
                if (
                    event.author == "user"
                    or event.content.role != "model"
                    or not call.id
                    or not call.name
                    or call.id in pending
                    or call.partial_args is not None
                    or call.will_continue
                    or type(call.args) is not dict
                ):
                    raise ValueError("native function call is partial, duplicated or unsupported")
                pending[call.id] = call.name
                has_call = True
            if part.function_response is not None:
                result = part.function_response
                if (
                    event.author == "user"
                    or event.content.role != "user"
                    or not result.id
                    or pending.get(result.id) != result.name
                    or result.will_continue
                    or result.scheduling is not None
                    or result.parts is not None
                    or type(result.response) is not dict
                ):
                    raise ValueError("native function result is unmatched or unsupported")
                del pending[result.id]
                has_response = True
        if event.author == "user":
            if event.content.role != "user" or has_call or has_response or not has_text or not text.strip():
                raise ValueError("native user event requires nonempty ordinary text")
            if any(part.thought or part.thought_signature is not None for part in event.content.parts):
                raise ValueError("native caller input cannot supply reasoning metadata")
            if event.usage_metadata is not None or event.model_version is not None:
                raise ValueError("native user event cannot report model usage")
            visible.append({"role": "user", "content": text})
        elif has_response:
            if has_call or has_text or event.usage_metadata is not None or event.model_version is not None:
                raise ValueError("native tool result cannot masquerade as a model response")
            last_model_text = False
        else:
            if event.content.role != "model" or pending_before:
                raise ValueError("native model response precedes unresolved tool results")
            native_usage(event.usage_metadata)
            model_events.append(event)
            if text:
                if visible and visible[-1]["role"] == "assistant":
                    visible[-1]["content"] += text
                else:
                    visible.append({"role": "assistant", "content": text})
            last_model_text = ordinary_text and bool(text.strip()) and not has_call
    if pending or not model_events or not last_model_text:
        raise ValueError("native session lacks a settled final text response")
    return native, visible, model_events


def _usage(value: Any) -> None:
    _keys(value, {"prompt_tokens", "completion_tokens", "total_tokens"}, {"model", "unmetered"})
    for key in ("prompt_tokens", "completion_tokens", "total_tokens"):
        _counter(value[key])
    if value["total_tokens"] != value["prompt_tokens"] + value["completion_tokens"]:
        raise ValueError("receipt token counts do not match")
    if "model" in value and (type(value["model"]) is not str or not value["model"].strip()):
        raise ValueError("receipt model must be nonempty")
    if "unmetered" in value and type(value["unmetered"]) is not bool:
        raise ValueError("receipt unmetered flag must be boolean")


def _validate_envelope(value: Any, *, graph_id: str, app_name: str, user_id: str, session_id: str) -> None:
    _identity(graph_id=graph_id, app_name=app_name, user_id=user_id, session_id=session_id)
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
    native, visible, model_events = _session(
        value["history"], app_name=app_name, user_id=user_id, session_id=session_id
    )
    if _canonical(messages) != _canonical(visible):
        raise ValueError("native session does not match its visible conversation")
    receipt = value["completed"]
    _keys(receipt, {"status", "run_id", "input", "output", "text_events"})
    if (
        receipt["status"] != "success"
        or type(receipt["run_id"]) is not str
        or not receipt["run_id"]
        or type(receipt["input"]) is not str
    ):
        raise ValueError("native state requires a successful committed receipt")
    if model_events[-1].invocation_id != receipt["run_id"]:
        raise ValueError("native latest invocation does not match its receipt")
    incoming = _messages(json.loads(receipt["input"], object_pairs_hook=_unique_object), incoming=True)
    if state_json(incoming) != receipt["input"]:
        raise ValueError("receipt input is not canonical JSON")
    text_events = receipt["text_events"]
    if type(text_events) is not list or any(type(item) is not str for item in text_events):
        raise ValueError("receipt text events must be strings")
    if len(state_json(text_events).encode()) > MAX_RECEIPT_BYTES:
        raise ValueError("Google ADK text receipt exceeds the 8 MiB limit")
    for text in text_events:
        if (
            len(state_json([{"id": f"{receipt['run_id']}:assistant", "type": "ai", "content": text}, {}]).encode())
            > MAX_OUTPUT_BYTES
        ):
            raise ValueError("Google ADK message exceeds the 3 MiB event limit")
    text = "".join(text_events)
    suffix = incoming + ([{"role": "assistant", "content": text}] if text else [])
    if len(messages) < len(suffix) or _canonical(messages[-len(suffix) :]) != _canonical(suffix):
        raise ValueError("receipt input/text does not match native history")
    output = receipt["output"]
    _keys(output, {"messages", "google_adk", "usage"})
    _keys(output["google_adk"], {"framework", "generation"})
    metadata = output["google_adk"]
    if _canonical(output["messages"]) != _canonical(messages) or metadata["framework"] != FRAMEWORK:
        raise ValueError("receipt output identity or history does not match")
    _counter(metadata["generation"])
    _usage(output["usage"])
    current = [event for event in model_events if event.invocation_id == receipt["run_id"]]
    prompt, completion = 0, 0
    for event in current:
        p, c = native_usage(event.usage_metadata)
        prompt, completion = prompt + p, completion + c
    _counter(prompt + completion)
    usage = output["usage"]
    if usage["prompt_tokens"] != prompt or usage["completion_tokens"] != completion:
        raise ValueError("receipt usage must describe only its native invocation")
    if "model" in usage and any(event.model_version != usage["model"] for event in current):
        raise ValueError("receipt model must identify every native model call")
    if len(state_json(output).encode()) > MAX_OUTPUT_BYTES:
        raise ValueError("Google ADK output exceeds the 3 MiB event limit")


def encode_state(value: Any, *, graph_id: str, app_name: str, user_id: str, session_id: str) -> bytes:
    """Validate deterministic native incompatibilities before attempting PUT."""
    body = state_json(value).encode("utf-8")
    if len(body) > MAX_STATE_BYTES:
        raise ValueError("Google ADK state exceeds the 16 MiB checkpoint limit")
    _validate_envelope(value, graph_id=graph_id, app_name=app_name, user_id=user_id, session_id=session_id)
    return body


class GoogleAdkState:
    def __init__(self, client: OpaqueCheckpointClient):
        self._client = client

    async def load(
        self, thread_id: str, *, graph_id: str, app_name: str, user_id: str, session_id: str
    ) -> tuple[dict[str, Any] | None, str | None]:
        try:
            _identity(graph_id=graph_id, app_name=app_name, user_id=user_id, session_id=session_id)
            if thread_id != session_id:
                raise ValueError("checkpoint thread and native session must match")
            raw, etag = await self._client.get_versioned(thread_id)
            if raw is None:
                return None, None
            if not _strong_numeric_etag(etag) or len(raw) > MAX_STATE_BYTES:
                raise ValueError("native state needs a valid version and bounded checkpoint")
            value = json.loads(raw.decode("utf-8"), object_pairs_hook=_unique_object)
            _validate_envelope(value, graph_id=graph_id, app_name=app_name, user_id=user_id, session_id=session_id)
            return value, etag
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            raise CompletionDeliveryError("Google ADK state could not be loaded safely") from exc

    async def save(
        self,
        thread_id: str,
        value: dict[str, Any],
        etag: str | None,
        *,
        graph_id: str,
        app_name: str,
        user_id: str,
        session_id: str,
    ) -> None:
        try:
            if thread_id != session_id:
                raise ValueError("checkpoint thread and native session must match")
            if etag is not None and not _strong_numeric_etag(etag):
                raise ValueError("native state requires a strong positive numeric ETag")
            body = encode_state(value, graph_id=graph_id, app_name=app_name, user_id=user_id, session_id=session_id)
            await self._client.put_cas(thread_id, body, etag=etag, create_only=etag is None)
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            raise CompletionDeliveryError("Google ADK state commit was not confirmed") from exc

    async def aclose(self) -> None:
        await self._client.aclose()
