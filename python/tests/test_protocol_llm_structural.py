"""Structural Runner Protocol checks against a *real* Gemini agent.

Unlike test_protocol_execute_goldens.py (exact expected_events vs a
scripted astream), this suite asserts invariants that stay true even
when model text / tool-call ids are non-deterministic:

1. lifecycle(running) first, end|error last, contiguous seq
2. known method vocabulary
3. a tool-using prompt yields at least one tool_call (or embedded tool_calls)
4. cancel mid-stream ends interrupted

Skipped automatically when GOOGLE_API_KEY / .env.llm is absent so CI
without secrets stays green.

Usage:
    set -a && source .env.llm && set +a
    PYTHONPATH=python python/.venv/bin/python python/tests/test_protocol_llm_structural.py
"""

from __future__ import annotations

import asyncio
import os
import sys
from pathlib import Path

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

ROOT = Path(__file__).resolve().parents[2]


def _load_dotenv_llm() -> None:
    path = ROOT / ".env.llm"
    if not path.is_file():
        return
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, _, v = line.partition("=")
        k, v = k.strip(), v.strip().strip('"').strip("'")
        if k and k not in os.environ:
            os.environ[k] = v


def check(name: str, cond: bool, detail: str = "") -> None:
    status = "PASS" if cond else "FAIL"
    suffix = f" -- {detail}" if detail and not cond else ""
    print(f"[{status}] {name}{suffix}")
    if not cond:
        raise SystemExit(1)


KNOWN_METHODS = {
    "lifecycle",
    "values",
    "updates",
    "messages",
    "end",
    "error",
    "tool_call",
    "input.requested",
}


def assert_structural(events: list[dict], *, expect_terminal: str) -> None:
    check("non-empty event stream", len(events) >= 2)
    check("first is lifecycle running", events[0].get("method") == "lifecycle" and (events[0].get("data") or {}).get("event") == "running")
    last = events[-1]
    check(f"last is {expect_terminal}", last.get("method") == expect_terminal or (expect_terminal == "end" and last.get("method") in ("end", "error")))
    for i, ev in enumerate(events):
        check(f"seq[{i}] contiguous", ev.get("seq") == i + 1, f"got {ev.get('seq')}")
        method = ev.get("method") or ""
        ok = method in KNOWN_METHODS or method.startswith("custom:")
        check(f"method[{i}] known ({method})", ok)
        check(f"namespace[{i}] is list", isinstance(ev.get("namespace"), list))


def _has_tool_signal(events: list[dict]) -> bool:
    for ev in events:
        if ev.get("method") == "tool_call":
            return True
        data = ev.get("data")
        blob = str(data)
        if "tool_calls" in blob or "tool_call_id" in blob:
            return True
    return False


async def run_once(assignment: dict, cancel_after_seq: int | None = None) -> list[dict]:
    from runkite_runner.worker import LangGraphAdapter, execute_run

    adapter = LangGraphAdapter()
    await adapter.load_config(str(ROOT / "examples/gemini/langgraph_agent/langgraph.json"))
    cancel_event = asyncio.Event() if cancel_after_seq is not None else None
    live: list[dict] = []

    async def on_event(ev: dict) -> None:
        live.append(ev)
        if cancel_event is not None and cancel_after_seq is not None and ev.get("seq") >= cancel_after_seq:
            cancel_event.set()

    await execute_run(adapter, assignment, on_event, cancel_event=cancel_event)
    return live


async def test_happy_structural() -> None:
    assignment = {
        "run_id": "struct-happy-0001",
        "thread_id": "struct-thread-0001",
        "runner_kind": "python-langgraph",
        "graph_id": "gemini_langgraph",
        "input": {"messages": [{"role": "user", "content": "Reply with exactly: pong"}]},
        "config": {"configurable": {}},
        "stream_modes": ["values", "updates"],
    }
    events = await run_once(assignment)
    assert_structural(events, expect_terminal="end")
    check("happy ends success", (events[-1].get("data") or {}).get("status") == "success")


async def test_tool_structural() -> None:
    # Real models occasionally skip tools despite instructions — one retry
    # keeps this structural (not exact-golden) check honest without flakes.
    last_events: list[dict] = []
    for attempt in range(2):
        assignment = {
            "run_id": f"struct-tool-000{attempt + 1}",
            "thread_id": f"struct-thread-tool-{attempt + 1}",
            "runner_kind": "python-langgraph",
            "graph_id": "gemini_langgraph",
            "input": {
                "messages": [
                    {
                        "role": "user",
                        "content": (
                            "You must call the search tool with query 'Runkite' "
                            "before answering. Do not answer without the tool."
                        ),
                    }
                ]
            },
            "config": {"configurable": {}},
            "stream_modes": ["values", "updates"],
        }
        last_events = await run_once(assignment)
        assert_structural(last_events, expect_terminal="end")
        if _has_tool_signal(last_events):
            check("tool-using prompt produced a tool signal", True)
            return
    check("tool-using prompt produced a tool signal", False, f"events={len(last_events)}")


async def test_cancel_structural() -> None:
    assignment = {
        "run_id": "struct-cancel-0001",
        "thread_id": "struct-thread-0003",
        "runner_kind": "python-langgraph",
        "graph_id": "gemini_langgraph",
        "input": {"messages": [{"role": "user", "content": "Write three short sentences about rivers."}]},
        "config": {"configurable": {}},
        "stream_modes": ["values", "updates"],
    }
    events = await run_once(assignment, cancel_after_seq=1)
    assert_structural(events, expect_terminal="end")
    check("cancel ends interrupted", (events[-1].get("data") or {}).get("status") == "interrupted")


async def main() -> None:
    _load_dotenv_llm()
    if not os.environ.get("GOOGLE_API_KEY"):
        print("SKIP: GOOGLE_API_KEY / .env.llm not set — structural LLM protocol suite not run")
        return
    await test_happy_structural()
    await test_tool_structural()
    await test_cancel_structural()
    print("\nAll structural LLM protocol checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
