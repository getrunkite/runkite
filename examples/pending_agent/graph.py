"""First-hour agent: always calls connector MCP refund through the plane.

No LLM. The plane's argument predicate holds refunds at or over 100 in
Admin Pending. After approve, send the same amount again to consume the
one-shot (digest-bound) retry.
"""

from __future__ import annotations

import re
from typing import Annotated, Any, TypedDict

from langchain_core.runnables import RunnableConfig
from langgraph.graph import END, START, StateGraph
from runkite_runner.connectors import ConnectorError, proxy_connector_mcp

_AMOUNT = re.compile(r"(\d+(?:\.\d+)?)")


class State(TypedDict):
    messages: Annotated[list[dict], lambda a, b: a + b]


def _amount_from(state: State) -> float:
    last = state["messages"][-1] if state.get("messages") else {}
    text = last.get("content") if isinstance(last, dict) else ""
    if not isinstance(text, str):
        text = str(text or "")
    m = _AMOUNT.search(text.replace(",", ""))
    if not m:
        return 500.0
    return float(m.group(1))


def _pending_text(body: dict[str, Any], amount: float) -> str:
    err = body.get("error") if isinstance(body, dict) else None
    data = {}
    if isinstance(err, dict) and isinstance(err.get("data"), dict):
        data = err["data"]
    code = data.get("reason_code") or ""
    action = data.get("action_id") or ""
    bits = [
        f"Held a ${amount:g} refund for a human ({code or 'pending'}).",
        "Open Admin → Pending, approve it, then send the same amount again.",
        "Approve is bound to these arguments; a different amount is a new row.",
    ]
    if action:
        bits.append(f"action_id={action}")
    return " ".join(bits)


async def refund_node(state: State, config: RunnableConfig) -> State:
    amount = _amount_from(state)
    try:
        body = await proxy_connector_mcp(
            config,
            "bank",
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "tools/call",
                "params": {
                    "name": "refund",
                    "arguments": {"amount": amount, "to": "vendor"},
                },
            },
        )
    except ConnectorError as e:
        # Policy pending/deny is HTTP 200 with JSON-RPC error in the body
        # (body.get("error") below). ConnectorError is transport or auth.
        return {"messages": [{"role": "ai", "content": f"Connector call failed: {e}"}]}

    if isinstance(body, dict) and body.get("error"):
        return {"messages": [{"role": "ai", "content": _pending_text(body, amount)}]}

    result = body.get("result") if isinstance(body, dict) else None
    text = ""
    if isinstance(result, dict):
        content = result.get("content")
        if isinstance(content, list) and content:
            first = content[0]
            if isinstance(first, dict):
                text = str(first.get("text") or "")
    if not text:
        text = f"fake bank accepted ${amount:g} refund (nothing moved)"
    return {"messages": [{"role": "ai", "content": text}]}


builder = StateGraph(State)
builder.add_node("refund", refund_node)
builder.add_edge(START, "refund")
builder.add_edge("refund", END)

graph = builder.compile()
