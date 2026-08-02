"""Execute-path golden harness for Runner Protocol examples/.

PROTOCOL.md §14.3 claims conformance by running a runner against the
fixtures under runner-protocol/examples/ with a deterministic mock agent
and diffing the live RunEvent stream against expected_events. The Go
fixtures_test.go gate only checks schema/lifecycle shape; this file is
the execute half.

For each fixture we:
1. Build a ScriptedGraph whose astream yields LangGraph-shaped chunks
   derived from the fixture's expected mid-stream events (not a
   passthrough of RunEvents -- execute_run still owns lifecycle/end/
   interrupt/cancel/error wrapping).
2. Drive real execute_run with that graph.
3. Normalize volatile fields (event_id, ts) and diff against goldens.

Usage:
    PYTHONPATH=python python/.venv/bin/python python/tests/test_protocol_execute_goldens.py
"""

from __future__ import annotations

import asyncio
import copy
import json
import os
import sys
from pathlib import Path
from typing import Any

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.worker import LangGraphAdapter, execute_run  # noqa: E402

EXAMPLES_DIR = Path(__file__).resolve().parents[2] / "runner-protocol" / "examples"


def check(name: str, cond: bool, detail: str = "") -> None:
    status = "PASS" if cond else "FAIL"
    suffix = f" -- {detail}" if detail and not cond else ""
    print(f"[{status}] {name}{suffix}")
    if not cond:
        raise SystemExit(1)


def normalize_event(ev: dict) -> dict:
    """Drop volatile fields; strip fixture-only `_note` keys recursively."""
    out = {
        "seq": ev["seq"],
        "method": ev["method"],
        "namespace": ev.get("namespace") or [],
        "data": _strip_notes(copy.deepcopy(ev.get("data"))),
    }
    return out


def _strip_notes(obj: Any) -> Any:
    if isinstance(obj, dict):
        return {k: _strip_notes(v) for k, v in obj.items() if k != "_note"}
    if isinstance(obj, list):
        return [_strip_notes(v) for v in obj]
    return obj


def normalize_expected(events: list[dict]) -> list[dict]:
    return [normalize_event(e) for e in events]


def _chunk_mode(chunk: Any, default_mode: str) -> str:
    if isinstance(chunk, tuple) and len(chunk) == 3:
        return chunk[1]
    if isinstance(chunk, tuple) and len(chunk) == 2:
        first, _second = chunk
        if isinstance(first, (tuple, list)) and not isinstance(first, (str, bytes)):
            return default_mode
        return first if isinstance(first, str) else default_mode
    return default_mode


class ScriptedGraph:
    """Minimal stand-in for a compiled LangGraph: only astream matters."""

    def __init__(self, chunks: list[Any], error: BaseException | None = None):
        self._chunks = chunks
        self._error = error

    async def astream(self, _input: Any, config: Any = None, stream_mode: Any = None):
        if self._error is not None:
            raise self._error
        if isinstance(stream_mode, list):
            modes = stream_mode
        elif stream_mode:
            modes = [stream_mode]
        else:
            modes = ["values"]
        default_mode = modes[0]
        for chunk in self._chunks:
            mode = _chunk_mode(chunk, default_mode)
            # Mirror LangGraph: astream only yields the requested modes.
            if mode not in modes:
                continue
            yield chunk
            # Yield to the event loop so cancel callbacks scheduled from
            # event_callback can land before the next chunk / loop exit.
            await asyncio.sleep(0)


class ScriptedAdapter(LangGraphAdapter):
    def __init__(self, graphs: dict[str, ScriptedGraph]):
        super().__init__()
        self.graphs = graphs


def _stream_method(method: str) -> bool:
    return method in ("values", "updates", "messages") or method.startswith("custom:")


def _chunk_for_event(ev: dict) -> Any:
    method = ev["method"]
    data = _strip_notes(copy.deepcopy(ev.get("data")))
    namespace = list(ev.get("namespace") or [])
    if method.startswith("custom:"):
        mode = "custom"
    else:
        mode = method
    if namespace:
        return (tuple(namespace), mode, data)
    return (mode, data)


def chunks_and_error_from_expected(
    expected: list[dict],
) -> tuple[list[Any], BaseException | None]:
    """Derive astream chunks + optional raise from expected_events.

    execute_run itself emits: initial lifecycle(running), interrupt
    lifecycle + input.requested (+ optional clean values), terminal
    end/error. Those are not replayed as graph chunks.
    """
    chunks: list[Any] = []
    error: BaseException | None = None
    i = 0
    n = len(expected)
    # Skip leading lifecycle(running)
    if i < n and expected[i].get("method") == "lifecycle" and (expected[i].get("data") or {}).get("event") == "running":
        i += 1

    while i < n:
        ev = expected[i]
        method = ev.get("method")

        if method == "error":
            data = ev.get("data") or {}
            msg = data.get("message") or "agent error"
            err_type = data.get("type") or "RuntimeError"
            # Match execute_run: message is str(exc), type is class name.
            cls = type(err_type, (Exception,), {})
            error = cls(msg)
            break

        if method in ("end",):
            break

        if method == "lifecycle" and (ev.get("data") or {}).get("event") == "interrupted":
            interrupts: list[dict] = []
            i += 1
            while i < n and expected[i].get("method") == "input.requested":
                req = expected[i].get("data") or {}
                interrupt: dict[str, Any] = {
                    "id": req.get("interrupt_id"),
                    "value": req.get("value"),
                }
                if "description" in req:
                    interrupt["description"] = req["description"]
                interrupts.append(interrupt)
                i += 1
            # Optional clean values/updates that fixtures place after
            # input.requested (execute_run emits those from the same
            # interrupt chunk's non-__interrupt__ keys).
            clean: dict[str, Any] = {}
            mode = "values"
            ns: list = []
            if i < n and _stream_method(expected[i].get("method", "")):
                # Only fold into the interrupt chunk when the fixture
                # puts stream data AFTER input.requested. Data before
                # the interrupt lifecycle is already in `chunks`.
                trailing = expected[i]
                mode = trailing["method"] if not trailing["method"].startswith("custom:") else "custom"
                clean = _strip_notes(copy.deepcopy(trailing.get("data"))) or {}
                ns = list(trailing.get("namespace") or [])
                i += 1
            payload = {"__interrupt__": interrupts, **clean}
            if ns:
                chunks.append((tuple(ns), mode, payload))
            else:
                chunks.append((mode, payload))
            continue

        if _stream_method(method or ""):
            chunks.append(_chunk_for_event(ev))
            i += 1
            continue

        # Unknown mid-stream method -- leave for the diff to fail loudly.
        i += 1

    return chunks, error


async def run_assignment(
    assignment: dict,
    expected: list[dict],
    *,
    cancel_after_seq: int | None = None,
) -> list[dict]:
    chunks, error = chunks_and_error_from_expected(expected)
    # RP-029: prove stream_mode filtering by injecting decoy values/updates
    # that must not appear in the live stream when only "messages" is requested.
    modes = assignment.get("stream_modes") or ["values"]
    if modes == ["messages"]:
        chunks = [
            ("values", {"messages": [{"role": "ai", "content": "should be filtered"}]}),
            ("updates", {"agent": {"messages": [{"role": "ai", "content": "should be filtered"}]}}),
            *chunks,
        ]
    graph_id = assignment["graph_id"]
    adapter = ScriptedAdapter({graph_id: ScriptedGraph(chunks, error=error)})
    cancel_event = asyncio.Event() if cancel_after_seq is not None else None
    live: list[dict] = []

    async def on_event(ev: dict) -> None:
        live.append(ev)
        if cancel_event is not None and cancel_after_seq is not None and ev.get("seq") >= cancel_after_seq:
            cancel_event.set()

    await execute_run(adapter, assignment, on_event, cancel_event=cancel_event)
    return live


def diff_events(live: list[dict], expected: list[dict], label: str) -> None:
    got = normalize_expected(live)
    want = normalize_expected(expected)
    if got == want:
        check(f"{label}: event stream matches golden", True)
        return
    # Compact mismatch report
    print(f"\n--- mismatch in {label} ---")
    print(f"got ({len(got)} events):")
    print(json.dumps(got, indent=2, default=str)[:4000])
    print(f"want ({len(want)} events):")
    print(json.dumps(want, indent=2, default=str)[:4000])
    check(f"{label}: event stream matches golden", False)


async def run_fixture(path: Path) -> None:
    doc = json.loads(path.read_text())
    test_id = doc.get("_test_id") or path.name
    label = f"{path.name} ({test_id})"

    if "assignment" in doc:
        cancel_after = doc.get("cancel_after_seq")
        cancel_after_seq = int(cancel_after) if cancel_after is not None else None
        live = await run_assignment(
            doc["assignment"],
            doc["expected_events"],
            cancel_after_seq=cancel_after_seq,
        )
        diff_events(live, doc["expected_events"], label)
        return

    if "run_1_assignment" in doc:
        live1 = await run_assignment(doc["run_1_assignment"], doc["run_1_expected_events"])
        diff_events(live1, doc["run_1_expected_events"], f"{label} run_1")
        live2 = await run_assignment(doc["run_2_assignment"], doc["run_2_expected_events"])
        diff_events(live2, doc["run_2_expected_events"], f"{label} run_2")
        return

    check(f"{label}: recognized fixture shape", False, "missing assignment / run_1_assignment")


async def main() -> None:
    files = sorted(p for p in EXAMPLES_DIR.glob("*.json") if p.is_file())
    check(f"found 10 protocol example fixtures (got {len(files)})", len(files) == 10)
    for path in files:
        await run_fixture(path)
    print("\nAll execute goldens passed.")


if __name__ == "__main__":
    asyncio.run(main())
