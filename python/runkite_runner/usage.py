"""Accumulate LLM token usage from LangGraph stream chunks into Output.usage.

FinOps (control plane) reads a top-level ``usage`` object on the final
``values`` blob that becomes ``run.Output``. LangChain / LangGraph put
token counts on ``AIMessage.usage_metadata`` (or legacy
``response_metadata["token_usage"]``); we sum those across the run and
emit the Agent Protocol–convention shape::

    {"prompt_tokens": N, "completion_tokens": M, "total_tokens": N+M, "model": "...", "cost_usd": X}

cost_usd is normally left unset here — the control plane pricebook fills
USD from tokens. The one exception: an LLM gateway sitting in front of the
provider (OpenRouter, and OpenAI-compatible gateways that follow the same
convention) can return an authoritative per-call cost inline in the same
usage object as the token counts (OpenRouter: ``usage.cost``, in USD).
LangChain's OpenAI-compatible client generally passes an unrecognized key
like this straight through into ``response_metadata.token_usage`` /
``usage_metadata`` without stripping it, so we opportunistically look for
it and — when present and non-zero — sum it into ``cost_usd``, which the
control plane's ``Pricebook.EstimateUSD`` already prefers over any
tokens × pricebook estimate. Gateways that report cost out-of-band instead
of inline (e.g. Portkey headers, Helicone's async dashboard) are not
covered by this — there is nothing in the message object to read.
"""

from __future__ import annotations

from typing import Any


def _as_int(v: Any) -> int:
    try:
        return int(v or 0)
    except (TypeError, ValueError):
        return 0


def _as_float(v: Any) -> float:
    try:
        return float(v or 0)
    except (TypeError, ValueError):
        return 0.0


def _usage_from_message(msg: Any) -> tuple[int, int, str, float]:
    """Return (prompt, completion, model, cost_usd) from one message-like object."""
    prompt = completion = 0
    model = ""
    cost = 0.0

    um = None
    if isinstance(msg, dict):
        um = msg.get("usage_metadata") or msg.get("usage")
        rm = msg.get("response_metadata") or {}
        if isinstance(rm, dict):
            model = str(rm.get("model_name") or rm.get("model") or "") or model
            tu = rm.get("token_usage") or rm.get("usage")
            if isinstance(tu, dict) and not um:
                um = tu
    else:
        um = getattr(msg, "usage_metadata", None)
        rm = getattr(msg, "response_metadata", None) or {}
        if isinstance(rm, dict):
            model = str(rm.get("model_name") or rm.get("model") or "") or model
            tu = rm.get("token_usage") or rm.get("usage")
            if isinstance(tu, dict) and not um:
                um = tu

    if isinstance(um, dict):
        prompt = _as_int(um.get("input_tokens") or um.get("prompt_tokens") or um.get("promptTokenCount"))
        completion = _as_int(um.get("output_tokens") or um.get("completion_tokens") or um.get("candidatesTokenCount"))
        if not prompt and not completion:
            # Some providers only report total.
            total = _as_int(um.get("total_tokens") or um.get("totalTokenCount"))
            if total:
                prompt = total
        if not model:
            model = str(um.get("model") or "")
        # Gateway-reported cost (OpenRouter: "cost"; some others: "total_cost").
        # 0/absent is the overwhelmingly common case (direct provider, no
        # gateway) — that is not an error, it just means the pricebook path
        # is what prices this run, same as before this field existed.
        cost = _as_float(um.get("cost") or um.get("total_cost"))

    return prompt, completion, model, cost


def _walk_messages(obj: Any) -> list[Any]:
    """Collect message-like objects from a values/updates/messages chunk."""
    out: list[Any] = []
    if obj is None:
        return out
    if isinstance(obj, (list, tuple)):
        for item in obj:
            out.extend(_walk_messages(item))
        return out
    if isinstance(obj, dict):
        # Serialized AIMessage dict — check this first: an AIMessage's own
        # dict form can itself contain a "messages"-named field in rare
        # framework-specific serializations, and this check must not be
        # skipped just because that key happens to exist.
        if obj.get("type") in ("ai", "AIMessage") or "usage_metadata" in obj:
            out.append(obj)
            return out
        # "values" mode: a state dict with a top-level "messages" key.
        if "messages" in obj:
            out.extend(_walk_messages(obj["messages"]))
            return out
        # "updates" mode: LangGraph wraps a superstep's changes as
        # {node_name: {...state changes...}}, one key per node that ran
        # (multiple keys when nodes fan out in parallel). There is no
        # "messages" key at this level — it is one level down, inside each
        # node's own update — so without this fallback, every run using
        # stream_modes=["updates"] alone reported zero usage regardless of
        # outcome (success, error, or interrupted): the messages were real,
        # accumulate_usage just never saw them under the wrong key.
        for v in obj.values():
            out.extend(_walk_messages(v))
        return out
    # LangChain BaseMessage
    t = getattr(obj, "type", None) or getattr(obj, "role", None)
    if t in ("ai", "assistant") or hasattr(obj, "usage_metadata"):
        out.append(obj)
    return out


def accumulate_usage(totals: dict[str, Any], data: Any) -> None:
    """Add any token usage found in *data* into *totals* (mutates).

    Call this on a single final ``values`` snapshot (or on incremental
    ``messages`` / ``updates`` chunks), not on every cumulative ``values``
    chunk — LangGraph ``values`` mode re-sends the full message history,
    so summing each snapshot double-counts prior AIMessage usage.
    """
    for msg in _walk_messages(data):
        p, c, model, cost = _usage_from_message(msg)
        if p or c:
            totals["prompt_tokens"] = int(totals.get("prompt_tokens") or 0) + p
            totals["completion_tokens"] = int(totals.get("completion_tokens") or 0) + c
        if cost:
            totals["cost_usd"] = float(totals.get("cost_usd") or 0) + cost
        if model and not totals.get("model"):
            totals["model"] = model


def usage_payload(totals: dict[str, Any]) -> dict[str, Any] | None:
    """Build the top-level Output.usage object, or None if empty."""
    p = int(totals.get("prompt_tokens") or 0)
    c = int(totals.get("completion_tokens") or 0)
    if p == 0 and c == 0:
        return None
    out: dict[str, Any] = {
        "prompt_tokens": p,
        "completion_tokens": c,
        "total_tokens": p + c,
    }
    if totals.get("model"):
        out["model"] = totals["model"]
    if totals.get("cost_usd"):
        out["cost_usd"] = totals["cost_usd"]
    return out


def usage_from_metrics(obj: Any) -> dict[str, Any] | None:
    """Normalize CrewAI/LlamaIndex/framework usage objects into Output.usage.

    Accepts dicts or objects with prompt/completion/total token attributes
    (CrewAI ``usage_metrics``, LlamaIndex response usage, etc.).
    """
    if obj is None:
        return None
    if not isinstance(obj, dict):
        # duck-type UsageMetrics / TokenUsage objects
        d: dict[str, Any] = {}
        for src, dest in (
            ("prompt_tokens", "prompt_tokens"),
            ("completion_tokens", "completion_tokens"),
            ("total_tokens", "total_tokens"),
            ("input_tokens", "prompt_tokens"),
            ("output_tokens", "completion_tokens"),
            ("successful_requests", None),
        ):
            if dest is None:
                continue
            if hasattr(obj, src):
                d[dest] = getattr(obj, src)
            elif isinstance(getattr(obj, "__dict__", None), dict) and src in obj.__dict__:
                d[dest] = obj.__dict__[src]
        # nested .token_usage
        tu = getattr(obj, "token_usage", None)
        if tu is not None and not d:
            return usage_from_metrics(tu)
        obj = d
    if not isinstance(obj, dict) or not obj:
        return None
    totals: dict[str, Any] = {}
    # map common keys into accumulate shape via a synthetic usage dict
    um = {
        "prompt_tokens": obj.get("prompt_tokens") or obj.get("input_tokens") or 0,
        "completion_tokens": obj.get("completion_tokens") or obj.get("output_tokens") or 0,
        "total_tokens": obj.get("total_tokens") or 0,
        "model": obj.get("model") or "",
    }
    p = _as_int(um["prompt_tokens"])
    c = _as_int(um["completion_tokens"])
    if not p and not c:
        total = _as_int(um["total_tokens"])
        if total:
            p = total
    if not p and not c:
        return None
    totals["prompt_tokens"] = p
    totals["completion_tokens"] = c
    if um.get("model"):
        totals["model"] = str(um["model"])
    return usage_payload(totals)


def values_with_usage(base: dict[str, Any], usage: dict[str, Any] | None) -> dict[str, Any]:
    """Return *base* values dict with top-level usage attached when present."""
    if not usage:
        return base
    out = dict(base)
    out["usage"] = usage
    return out
