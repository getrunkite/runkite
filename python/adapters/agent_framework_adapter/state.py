"""CAS-backed native session + committed-turn receipts for Agent Framework."""

from __future__ import annotations

import json
from typing import Any

from runkite_runner.completion import CompletionDeliveryError
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient

FRAMEWORK = "agent-framework"
MAX_STATE_BYTES = 16 << 20


def state_json(value: Any) -> str:
    """Strict JSON for durable state: no NaN, cycles, or non-JSON types."""
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)


class AgentFrameworkState:
    """Load/save adapter-state with If-Match fencing, matching the AI SDK path."""

    def __init__(self, client: OpaqueCheckpointClient):
        self._client = client

    async def load(self, thread_id: str) -> tuple[dict[str, Any] | None, str | None]:
        try:
            raw, etag = await self._client.get_versioned(thread_id)
            if raw is None:
                return None, None
            if len(raw) > MAX_STATE_BYTES:
                raise ValueError("Agent Framework state exceeds the opaque checkpoint limit")
            value = json.loads(raw.decode("utf-8"))
            if not isinstance(value, dict):
                raise ValueError("Agent Framework state must be a JSON object")
            if value.get("framework") not in (None, FRAMEWORK):
                raise ValueError("thread contains another framework's state")
            state_json(value)
            return value, etag
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            raise CompletionDeliveryError("Agent Framework state could not be loaded safely") from exc

    async def save(self, thread_id: str, value: dict[str, Any], etag: str | None) -> None:
        try:
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
