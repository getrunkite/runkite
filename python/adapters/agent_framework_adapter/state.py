"""CAS-backed native session + committed-turn receipts for Agent Framework."""

from __future__ import annotations

import json
from typing import Any

from runkite_runner.completion import CompletionDeliveryError
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient, _strong_numeric_etag

FRAMEWORK = "agent-framework"
MAX_STATE_BYTES = 16 << 20


def state_json(value: Any) -> str:
    """Serialize native JSON without lossy key conversion or invalid Unicode."""
    ancestors: set[int] = set()

    def validate(item: Any) -> None:
        if item is None or type(item) in (str, bool, int, float):
            return
        if type(item) not in (dict, list) or id(item) in ancestors:
            raise ValueError("state contains a non-JSON value or cycle")
        ancestors.add(id(item))
        try:
            if isinstance(item, dict):
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


def _messages(value: Any, *, incoming: bool = False) -> list[dict[str, str]]:
    if not isinstance(value, list) or (incoming and not value):
        raise ValueError("state messages must be an array")
    roles = {"user", "system"} if incoming else {"user", "system", "assistant"}
    for message in value:
        if (
            not isinstance(message, dict)
            or message.get("role") not in roles
            or not isinstance(message.get("content"), str)
        ):
            raise ValueError("state contains an invalid text message")
    if incoming and value[-1]["role"] != "user":
        raise ValueError("receipt input must end in a user message")
    return value


def _validate_envelope(value: Any, graph_id: str) -> None:
    from agent_framework import AgentSession, Content, Message

    if not isinstance(graph_id, str) or not graph_id:
        raise ValueError("a trusted graph_id is required for native state")
    if (
        not isinstance(value, dict)
        or type(value.get("v")) is not int
        or value["v"] != 1
        or value.get("framework") != FRAMEWORK
        or value.get("graph_id") != graph_id
    ):
        raise ValueError("native state version, framework or graph does not match")
    state_json(value)
    messages = _messages(value.get("messages"))
    session = value.get("session")
    if (
        not isinstance(session, dict)
        or session.get("type") != "session"
        or not isinstance(session.get("session_id"), str)
        or not session["session_id"]
        or not isinstance(session.get("state"), dict)
        or not isinstance(session.get("service_session_id"), (str, dict, type(None)))
    ):
        raise ValueError("native state requires a valid AgentSession")

    # Inspect decoded public native values, not arbitrary JSON with similar keys.
    def complete_messages(messages: list[Message]) -> None:
        unresolved: set[str] = set()
        for message in messages:
            for content in message.contents or []:
                reject_approval(content)
                if content.type == "function_call" and not content.informational_only:
                    if not isinstance(content.call_id, str) or not content.call_id:
                        raise ValueError("native tool calls require a call ID")
                    unresolved.add(content.call_id)
                elif content.type == "function_result":
                    unresolved.discard(content.call_id)
        if unresolved:
            raise ValueError("native state contains unresolved tool calls")

    def reject_approval(item: Any) -> None:
        if isinstance(item, Content):
            if item.user_input_request or item.type == "function_approval_response":
                raise ValueError("native approval and user-input continuation are unsupported")
        elif isinstance(item, Message):
            complete_messages([item])
        elif isinstance(item, dict):
            for child in item.values():
                reject_approval(child)
        elif isinstance(item, list):
            if all(isinstance(child, Message) for child in item):
                complete_messages(item)
            else:
                for child in item:
                    reject_approval(child)

    restored = AgentSession.from_dict(session)
    reject_approval(restored.state)
    state_json(restored.to_dict())
    completed = value.get("completed")
    if (
        not isinstance(completed, dict)
        or completed.get("status") != "success"
        or not isinstance(completed.get("run_id"), str)
        or not completed["run_id"]
        or not isinstance(completed.get("input"), str)
        or not isinstance(completed.get("text_events"), list)
        or any(not isinstance(delta, str) for delta in completed["text_events"])
        or not isinstance(completed.get("output"), dict)
    ):
        raise ValueError("native state requires a valid successful receipt")
    incoming = _messages(json.loads(completed["input"], object_pairs_hook=_unique_object), incoming=True)
    if state_json(incoming) != completed["input"]:
        raise ValueError("receipt input is not canonical JSON")
    text = "".join(completed["text_events"])
    suffix = incoming + ([{"role": "assistant", "content": text}] if text else [])
    if len(messages) < len(suffix) or messages[-len(suffix) :] != suffix:
        raise ValueError("receipt input/text does not match its conversation history")
    output = completed["output"]
    metadata = output.get("agent_framework")
    if (
        output.get("messages") != messages
        or not isinstance(metadata, dict)
        or metadata.get("framework") != FRAMEWORK
        or type(metadata.get("generation")) is not int
        or metadata["generation"] < 0
        or metadata.get("session_id") != session["session_id"]
    ):
        raise ValueError("receipt output does not match its native session")
    if "usage" in output:
        usage = output["usage"]
        if not isinstance(usage, dict) or any(
            type(usage.get(key)) is not int or usage[key] < 0
            for key in ("prompt_tokens", "completion_tokens", "total_tokens")
        ):
            raise ValueError("receipt usage must contain nonnegative token counts")
        if usage["total_tokens"] != usage["prompt_tokens"] + usage["completion_tokens"]:
            raise ValueError("receipt usage token counts do not match")
        if "unmetered" in usage and type(usage["unmetered"]) is not bool:
            raise ValueError("receipt unmetered flag must be a boolean")
        if "model" in usage and not isinstance(usage["model"], str):
            raise ValueError("receipt model must be a string")
        if "cost_usd" in usage and (type(usage["cost_usd"]) not in (int, float) or usage["cost_usd"] < 0):
            raise ValueError("receipt cost must be a nonnegative number")


class AgentFrameworkState:
    """Load/save adapter-state with If-Match fencing, matching the AI SDK path."""

    def __init__(self, client: OpaqueCheckpointClient):
        self._client = client

    async def load(self, thread_id: str, *, graph_id: str) -> tuple[dict[str, Any] | None, str | None]:
        try:
            raw, etag = await self._client.get_versioned(thread_id)
            if raw is None:
                return None, None
            if not _strong_numeric_etag(etag):
                raise ValueError("native state requires a strong positive numeric ETag")
            if len(raw) > MAX_STATE_BYTES:
                raise ValueError("Agent Framework state exceeds the opaque checkpoint limit")
            value = json.loads(raw.decode("utf-8"), object_pairs_hook=_unique_object)
            _validate_envelope(value, graph_id)
            return value, etag
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            raise CompletionDeliveryError("Agent Framework state could not be loaded safely") from exc

    async def save(self, thread_id: str, value: dict[str, Any], etag: str | None, *, graph_id: str) -> None:
        try:
            _validate_envelope(value, graph_id)
            body = state_json(value).encode("utf-8")
            if len(body) > MAX_STATE_BYTES:
                raise ValueError("Agent Framework state exceeds the opaque checkpoint limit")
            await self._client.put_cas(
                thread_id,
                body,
                etag=etag,
                create_only=etag is None,
            )
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            raise CompletionDeliveryError("Agent Framework state commit was not confirmed") from exc

    async def aclose(self) -> None:
        await self._client.aclose()
