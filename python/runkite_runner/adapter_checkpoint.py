"""Shared message-history blob helpers for non-LangGraph opaque checkpoints.

Blob format (opaque to the control plane):
  {"v": 1, "messages": [{"role": "...", "content": "..."}, ...]}

Adapters that only need durable Agent Protocol multi-turn (client sends
the new message; plane restores prior turns) reuse encode/decode/merge.
"""

from __future__ import annotations

import json
from typing import Any

BLOB_VERSION = 1


def encode_messages_checkpoint(messages: list) -> bytes:
    return json.dumps({"v": BLOB_VERSION, "messages": list(messages)}, separators=(",", ":")).encode("utf-8")


def decode_messages_checkpoint(data: bytes | None) -> list:
    if not data:
        return []
    raw = json.loads(data.decode("utf-8"))
    if not isinstance(raw, dict):
        return []
    msgs = raw.get("messages")
    return list(msgs) if isinstance(msgs, list) else []


def merge_messages_input(prior_messages: list, input_data: dict) -> dict:
    """Prepend restored history when the client sends only the new turn(s).

    If the client already sent a history at least as long as the checkpoint,
    prefer the client payload (explicit full history — avoids double-prepend
    when lengths match).
    """
    incoming = list((input_data or {}).get("messages") or [])
    if not prior_messages:
        return dict(input_data or {})
    if len(incoming) >= len(prior_messages):
        # Client-managed history (equal or longer) — don't double-prepend.
        return dict(input_data or {})
    merged = {**(input_data or {}), "messages": list(prior_messages) + incoming}
    return merged


def format_messages_as_context(messages: list) -> str:
    """Flatten prior+current turns for last-message-only frameworks (CrewAI, etc.)."""
    lines: list[str] = []
    for msg in messages:
        if not isinstance(msg, dict):
            continue
        role = msg.get("role") or msg.get("type") or "user"
        content = msg.get("content")
        if isinstance(content, list):
            content = " ".join(b.get("text", "") for b in content if isinstance(b, dict) and b.get("type") == "text")
        if content is None:
            content = ""
        lines.append(f"{role}: {content}")
    return "\n".join(lines)


def last_human_text(messages: list) -> str:
    for msg in reversed(messages):
        role = msg.get("role") or msg.get("type") if isinstance(msg, dict) else getattr(msg, "type", None)
        if role in ("human", "user"):
            content = msg.get("content") if isinstance(msg, dict) else getattr(msg, "content", None)
            if isinstance(content, str):
                return content
            if isinstance(content, list):
                parts = [b.get("text", "") for b in content if isinstance(b, dict) and b.get("type") == "text"]
                return " ".join(parts)
    return ""


def context_prompt_from_messages(messages: list) -> str:
    """For adapters that invoke with a single string: include prior turns."""
    if len(messages) <= 1:
        return last_human_text(messages)
    prior = messages[:-1]
    last = last_human_text(messages)
    hist = format_messages_as_context(prior)
    if not hist:
        return last
    return f"Previous conversation:\n{hist}\n\nCurrent message:\n{last}"


def messages_from_values_event(values: dict[str, Any] | None) -> list | None:
    if not values:
        return None
    msgs = values.get("messages")
    return list(msgs) if isinstance(msgs, list) else None
