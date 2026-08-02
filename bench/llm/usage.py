"""Extract token usage from Runner Protocol / LangChain-shaped event payloads.

Live agents don't always surface Gemini usage_metadata on the wire; when
they do, we prefer it. When they don't, callers should charge a
*conservative* estimate (over-count) so the soft budget trips early
rather than late on long agentic loops.
"""

from __future__ import annotations

from typing import Any


def _as_int(v: Any) -> int | None:
    if v is None:
        return None
    if isinstance(v, bool):
        return None
    if isinstance(v, int):
        return v
    if isinstance(v, float) and v == int(v):
        return int(v)
    if isinstance(v, str) and v.isdigit():
        return int(v)
    return None


def _from_usage_dict(d: dict) -> tuple[int, int] | None:
    # Google genai / LangChain shapes seen in the wild.
    prompt = (
        _as_int(d.get("prompt_tokens"))
        or _as_int(d.get("prompt_token_count"))
        or _as_int(d.get("input_tokens"))
        or _as_int(d.get("inputTokenCount"))
    )
    completion = (
        _as_int(d.get("completion_tokens"))
        or _as_int(d.get("candidates_token_count"))
        or _as_int(d.get("output_tokens"))
        or _as_int(d.get("outputTokenCount"))
        or _as_int(d.get("completion_token_count"))
    )
    total = _as_int(d.get("total_tokens")) or _as_int(d.get("total_token_count"))
    if prompt is None and completion is None and total is None:
        return None
    if prompt is None and completion is None and total is not None:
        # Unknown split — attribute all to prompt (conservative for cost
        # if output rate > input rate; still better than inventing both).
        return total, 0
    return prompt or 0, completion or 0


def extract_usage_from_obj(obj: Any, acc: list[tuple[int, int]] | None = None) -> list[tuple[int, int]]:
    if acc is None:
        acc = []
    if isinstance(obj, dict):
        for key in ("usage_metadata", "usage", "token_usage"):
            if isinstance(obj.get(key), dict):
                got = _from_usage_dict(obj[key])
                if got:
                    acc.append(got)
        rm = obj.get("response_metadata")
        if isinstance(rm, dict):
            for key in ("usage_metadata", "token_usage", "usage"):
                if isinstance(rm.get(key), dict):
                    got = _from_usage_dict(rm[key])
                    if got:
                        acc.append(got)
        for v in obj.values():
            extract_usage_from_obj(v, acc)
    elif isinstance(obj, list):
        for item in obj:
            extract_usage_from_obj(item, acc)
    return acc


def sum_usage_from_events(events: list[dict]) -> tuple[int, int, bool]:
    """Return (prompt_tokens, completion_tokens, measured).

    measured=True only when at least one usage block was found in the stream.
    Walk each event once (not data then full event) to avoid double-counting.
    """
    found: list[tuple[int, int]] = []
    for ev in events:
        extract_usage_from_obj(ev, found)
    if not found:
        return 0, 0, False
    prompt = sum(p for p, _ in found)
    completion = sum(c for _, c in found)
    return prompt, completion, True
