"""Fenced native input history and successful-turn receipts for OpenAI Agents."""

from __future__ import annotations

import json
from functools import lru_cache
from typing import Any

from runkite_runner.completion import CompletionDeliveryError
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient, _strong_numeric_etag

FRAMEWORK = "openai-agents"
MAX_OUTPUT_BYTES = 3 << 20
MAX_RECEIPT_BYTES = 8 << 20
MAX_STATE_BYTES = 16 << 20
MAX_INT64 = (1 << 63) - 1


def state_json(value: Any) -> str:
    """Encode only lossless JSON, including finite numbers and valid UTF-8."""
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


def _messages(value: Any, *, incoming: bool = False) -> list[dict[str, str]]:
    if type(value) is not list or not value:
        raise ValueError("state requires text messages")
    roles = {"user", "system"} if incoming else {"user", "system", "assistant"}
    for message in value:
        _keys(message, {"role", "content"})
        if message["role"] not in roles or type(message["content"]) is not str:
            raise ValueError("state contains an invalid text message")
    if incoming and (value[-1]["role"] != "user" or not value[-1]["content"].strip()):
        raise ValueError("receipt input must end in a nonempty user message")
    return value


@lru_cache(maxsize=1)
def _native_adapters() -> dict[str, Any]:
    # Use the SDK's public Responses input schemas. Some fields are Iterables,
    # so validation must be materialized before accepting the persisted value.
    from openai.types.responses import (
        EasyInputMessageParam,
        ResponseFunctionToolCallParam,
        ResponseOutputMessageParam,
        ResponseReasoningItemParam,
    )
    from openai.types.responses.response_input_item_param import FunctionCallOutput
    from pydantic import TypeAdapter

    return {
        "input": TypeAdapter(EasyInputMessageParam),
        "message": TypeAdapter(ResponseOutputMessageParam),
        "function_call": TypeAdapter(ResponseFunctionToolCallParam),
        "function_call_output": TypeAdapter(FunctionCallOutput),
        "reasoning": TypeAdapter(ResponseReasoningItemParam),
    }


def _native_item(value: dict[str, Any], kind: str) -> None:
    # The Agents SDK's chat-completions codec adds provider_data to its public
    # native history. Preserve this JSON metadata while validating the standard
    # Responses item separately. Other unknown item fields still fail closed.
    schema_value = value
    if "provider_data" in value:
        if type(value["provider_data"]) is not dict:
            raise ValueError("native SDK provider_data must be a JSON object")
        state_json(value["provider_data"])
        schema_value = {key: item for key, item in value.items() if key != "provider_data"}
    adapter = _native_adapters()[kind]
    validated = adapter.validate_json(state_json(schema_value), strict=True)
    materialized = json.loads(adapter.dump_json(validated, warnings="error"))
    if materialized != schema_value:
        raise ValueError("native history cannot be restored without losing fields")


def _native_messages(history: Any) -> list[dict[str, str]]:
    """Project supported public native history without losing its tool records."""
    if type(history) is not list or not history:
        raise ValueError("native history must be a nonempty array")
    messages: list[dict[str, str]] = []
    pending: set[str] = set()
    seen_calls: set[str] = set()
    turn_has_user = False
    last_non_reasoning: str | None = None
    for item in history:
        if type(item) is not dict:
            raise ValueError("native history items must be JSON objects")
        kind = item.get("type")
        if kind in (None, "message") and item.get("role") in ("user", "system"):
            _native_item(item, "input")
            if type(item.get("content")) is not str or pending:
                raise ValueError("native input must be text and cannot interrupt tool calls")
            messages.append({"role": item["role"], "content": item["content"]})
            turn_has_user = item["role"] == "user"
            last_non_reasoning = "input"
        elif kind == "message":
            _native_item(item, kind)
            if not turn_has_user or item.get("status") != "completed" or not item.get("id"):
                raise ValueError("native assistant message is not complete")
            parts = item["content"]
            if any(part.get("type") not in ("output_text", "refusal") for part in parts):
                raise ValueError("native assistant messages support text output only")
            text = "".join(part["text"] if part["type"] == "output_text" else part["refusal"] for part in parts)
            if text:
                if messages and messages[-1]["role"] == "assistant":
                    messages[-1]["content"] += text
                else:
                    messages.append({"role": "assistant", "content": text})
            last_non_reasoning = kind
        elif kind in ("function_call", "function_call_output"):
            _native_item(item, kind)
            if not turn_has_user or item.get("status") not in (None, "completed") or item.get("async", False):
                raise ValueError("native asynchronous or unfinished tools are unsupported")
            caller = item.get("caller")
            if caller is not None and caller.get("type") != "direct":
                raise ValueError("native history supports local function calls only")
            call_id = item.get("call_id")
            if type(call_id) is not str or not call_id:
                raise ValueError("native tool calls require a call ID")
            if kind == "function_call":
                if not item["name"] or call_id in seen_calls:
                    raise ValueError("native tool call IDs must be unique and names nonempty")
                seen_calls.add(call_id)
                pending.add(call_id)
            else:
                output = item["output"]
                text_output = type(output) is str or (
                    type(output) is list
                    and all(part.get("type") == "input_text" and type(part.get("text")) is str for part in output)
                )
                if call_id not in pending or not text_output:
                    raise ValueError("native tool output must be text with a matching pending call")
                pending.remove(call_id)
            last_non_reasoning = kind
        elif kind == "reasoning":
            _native_item(item, kind)
            if not turn_has_user or not item.get("id") or item.get("status") not in (None, "completed"):
                raise ValueError("native reasoning item is not complete")
        else:
            raise ValueError("native history contains an unsupported item type")
    if pending:
        raise ValueError("native history contains unresolved function calls")
    if last_non_reasoning != "message":
        raise ValueError("native history requires a completed final assistant message")
    return messages


def _usage(value: Any) -> None:
    _keys(value, {"prompt_tokens", "completion_tokens", "total_tokens"}, {"model", "cost_usd", "unmetered"})
    if any(
        type(value[key]) is not int or not 0 <= value[key] <= MAX_INT64
        for key in ("prompt_tokens", "completion_tokens", "total_tokens")
    ):
        raise ValueError("receipt usage must contain nonnegative int64 token counts")
    if value["total_tokens"] != value["prompt_tokens"] + value["completion_tokens"]:
        raise ValueError("receipt usage token counts do not match")
    if "unmetered" in value and type(value["unmetered"]) is not bool:
        raise ValueError("receipt unmetered flag must be a boolean")
    if "model" in value and type(value["model"]) is not str:
        raise ValueError("receipt model must be a string")
    if "cost_usd" in value and (type(value["cost_usd"]) not in (int, float) or value["cost_usd"] < 0):
        raise ValueError("receipt cost must be a nonnegative number")


def _validate_envelope(value: Any, graph_id: str) -> None:
    if type(graph_id) is not str or not graph_id:
        raise ValueError("a trusted graph_id is required for native state")
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
    if _native_messages(value["history"]) != messages:
        raise ValueError("native history does not match its plain conversation")
    receipt = value["completed"]
    _keys(receipt, {"status", "run_id", "input", "output", "text_events"})
    if receipt["status"] != "success" or type(receipt["run_id"]) is not str or not receipt["run_id"]:
        raise ValueError("native state requires a successful committed-turn receipt")
    if type(receipt["input"]) is not str:
        raise ValueError("receipt input must be canonical JSON")
    incoming = _messages(json.loads(receipt["input"], object_pairs_hook=_unique_object), incoming=True)
    if state_json(incoming) != receipt["input"]:
        raise ValueError("receipt input is not canonical JSON")
    text_events = receipt["text_events"]
    if type(text_events) is not list or any(type(delta) is not str for delta in text_events):
        raise ValueError("receipt text events must contain strings")
    if len(state_json(text_events).encode("utf-8")) > MAX_RECEIPT_BYTES:
        raise ValueError("OpenAI Agents text receipt exceeds the 8 MiB limit")
    for delta in text_events:
        payload = [{"id": f"{receipt['run_id']}:assistant", "type": "ai", "content": delta}, {}]
        if len(state_json(payload).encode("utf-8")) > MAX_OUTPUT_BYTES:
            raise ValueError("OpenAI Agents message exceeds the 3 MiB event limit")
    text = "".join(text_events)
    suffix = incoming + ([{"role": "assistant", "content": text}] if text else [])
    if len(messages) < len(suffix) or messages[-len(suffix) :] != suffix:
        raise ValueError("receipt input/text does not match its conversation history")
    output = receipt["output"]
    _keys(output, {"messages", "openai_agents"}, {"usage"})
    _keys(output["openai_agents"], {"framework", "generation"})
    metadata = output["openai_agents"]
    if (
        output["messages"] != messages
        or metadata["framework"] != FRAMEWORK
        or type(metadata["generation"]) is not int
        or not 0 <= metadata["generation"] <= MAX_INT64
    ):
        raise ValueError("receipt output does not match its native history")
    if "usage" in output:
        _usage(output["usage"])
    if len(state_json(output).encode("utf-8")) > MAX_OUTPUT_BYTES:
        raise ValueError("OpenAI Agents output exceeds the 3 MiB event limit")


def encode_state(value: Any, *, graph_id: str) -> bytes:
    """Validate locally produced state before committing or asking for a retry."""
    body = state_json(value).encode("utf-8")
    if len(body) > MAX_STATE_BYTES:
        raise ValueError("OpenAI Agents state exceeds the 16 MiB checkpoint limit")
    _validate_envelope(value, graph_id)
    return body


class OpenAIAgentsState:
    """Persist native input items with framework, agent and attempt fencing."""

    def __init__(self, client: OpaqueCheckpointClient):
        self._client = client

    async def load(self, thread_id: str, *, graph_id: str) -> tuple[dict[str, Any] | None, str | None]:
        try:
            if type(graph_id) is not str or not graph_id:
                raise ValueError("a trusted graph_id is required for native state")
            raw, etag = await self._client.get_versioned(thread_id)
            if raw is None:
                return None, None
            if not _strong_numeric_etag(etag):
                raise ValueError("native state requires a strong positive numeric ETag")
            if len(raw) > MAX_STATE_BYTES:
                raise ValueError("OpenAI Agents state exceeds the 16 MiB checkpoint limit")
            value = json.loads(raw.decode("utf-8"), object_pairs_hook=_unique_object)
            _validate_envelope(value, graph_id)
            return value, etag
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            raise CompletionDeliveryError("OpenAI Agents state could not be loaded safely") from exc

    async def save(self, thread_id: str, value: dict[str, Any], etag: str | None, *, graph_id: str) -> None:
        try:
            if etag is not None and not _strong_numeric_etag(etag):
                raise ValueError("native state requires a strong positive numeric ETag")
            body = encode_state(value, graph_id=graph_id)
            await self._client.put_cas(thread_id, body, etag=etag, create_only=etag is None)
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            raise CompletionDeliveryError("OpenAI Agents state commit was not confirmed") from exc

    async def aclose(self) -> None:
        await self._client.aclose()
