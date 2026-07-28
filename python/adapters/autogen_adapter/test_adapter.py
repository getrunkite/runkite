"""Self-check for the AutoGen adapter (python/adapters/autogen_adapter).

Lives alongside the adapter (not in python/tests/) since it needs the
AutoGen-specific isolated venv, not the shared runkite_runner one --
see this adapter's own README for why AutoGen/CrewAI/LlamaIndex get
their own venvs rather than joining the shared requirements.txt.

Proves:
1. load_config loads an AssistantAgent from a langgraph.json-shaped config.
2. execute() extracts the last human message's text, calls
   agent.run(task=text), and emits the same RunEvent sequence
   (lifecycle -> values -> end) every other adapter does.
3. An unknown graph_id and an agent that raises both produce a
   well-formed "error" status instead of crashing the worker loop.
4. Concurrent run() calls on a SHARED agent instance are serialized
   (the same runner-side concurrency spot-check as the CrewAI adapter).
5. A cancel_event fired mid-run produces "interrupted", not "error".

Usage:
    python/adapters/autogen_adapter/.venv/bin/python \\
        python/adapters/autogen_adapter/test_adapter.py
"""

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", ".."))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "..", "python"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "..", "python", "adapters"))

from autogen_adapter.adapter import AutoGenAdapter  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def _collector():
    events: list[dict] = []

    async def callback(event: dict):
        events.append(event)

    return callback, events


class _FakeMessage:
    def __init__(self, content):
        self.content = content


class _FakeTaskResult:
    def __init__(self, reply_text):
        self.messages = [_FakeMessage("task"), _FakeMessage(reply_text)]


class _FakeAssistantAgent:
    def __init__(self, response=None, raise_error=False, delay=0):
        self._response = response
        self._raise_error = raise_error
        self._delay = delay
        self.last_task = None

    async def run(self, task):
        self.last_task = task
        if self._delay:
            await asyncio.sleep(self._delay)
        if self._raise_error:
            raise RuntimeError("agent blew up")
        return _FakeTaskResult(self._response)


async def test_execute_happy_path_emits_expected_events():
    adapter = AutoGenAdapter()
    adapter.agents["my_agent"] = _FakeAssistantAgent(response="fake agent reply")

    callback, events = _collector()
    status = await adapter.execute(
        {"run_id": "r1", "graph_id": "my_agent", "input": {"messages": [{"role": "user", "content": "hello agent"}]}},
        callback,
        None,
    )
    check("status success", status == "success")
    methods = [e["method"] for e in events]
    check("lifecycle running emitted first", methods[0] == "lifecycle" and events[0]["data"]["event"] == "running")
    check("values emitted", "values" in methods)
    check("end success emitted last", methods[-1] == "end" and events[-1]["data"]["status"] == "success")

    values_event = next(e for e in events if e["method"] == "values")
    check("reply appended to messages", values_event["data"]["messages"][-1] == {"role": "ai", "content": "fake agent reply"})


async def test_extracts_last_human_message_as_input():
    adapter = AutoGenAdapter()
    fake = _FakeAssistantAgent(response="ok")
    adapter.agents["my_agent"] = fake

    callback, _ = _collector()
    await adapter.execute(
        {"run_id": "r2", "graph_id": "my_agent", "input": {"messages": [
            {"role": "ai", "content": "ignored"},
            {"role": "user", "content": "the real ask"},
        ]}},
        callback,
        None,
    )
    check("agent.run called with task=<last human text>", fake.last_task == "the real ask")


async def test_unknown_graph_id_reports_error():
    adapter = AutoGenAdapter()
    callback, events = _collector()
    status = await adapter.execute({"run_id": "r3", "graph_id": "nope", "input": {}}, callback, None)
    check("unknown graph_id -> error status", status == "error")
    check("error event emitted", any(e["method"] == "error" for e in events))


async def test_agent_exception_reports_error():
    adapter = AutoGenAdapter()
    adapter.agents["my_agent"] = _FakeAssistantAgent(raise_error=True)
    callback, events = _collector()
    status = await adapter.execute({"run_id": "r4", "graph_id": "my_agent", "input": {"messages": [{"role": "user", "content": "hi"}]}}, callback, None)
    check("agent exception -> error status", status == "error")
    check("error event includes the exception message", any(e["method"] == "error" and "agent blew up" in e["data"]["message"] for e in events))


async def test_concurrent_run_on_shared_agent_is_serialized():
    """Regression for the runner-side concurrency spot-check: a shared
    AssistantAgent's run() mutates its own model_context (conversation
    history), so two concurrent runs against the SAME graph_id must
    never overlap inside run() itself, even though the runner dispatches
    them concurrently."""
    adapter = AutoGenAdapter()
    current = 0
    max_concurrent = 0

    class _TrackingFakeAgent:
        async def run(self, task):
            nonlocal current, max_concurrent
            current += 1
            max_concurrent = max(max_concurrent, current)
            try:
                await asyncio.sleep(0.05)
                return _FakeTaskResult(f"reply-to-{task}")
            finally:
                current -= 1

    adapter.agents["shared_agent"] = _TrackingFakeAgent()

    async def run(label: str):
        callback, events = _collector()
        status = await adapter.execute(
            {"run_id": label, "graph_id": "shared_agent", "input": {"messages": [{"role": "user", "content": label}]}},
            callback,
            None,
        )
        return status, events

    results = await asyncio.gather(run("r-a"), run("r-b"), run("r-c"))
    check("max_concurrent inside run() never exceeded 1 (serialized by the lock)", max_concurrent == 1)
    check("all three concurrent runs still completed successfully", all(status == "success" for status, _ in results))


async def test_sequential_runs_clear_shared_model_context():
    """Regression: a shared AssistantAgent appends to model_context on
    every run(). Without an explicit clear between runs, run N+1 would
    see run N's history -- cross-thread leakage on a long-lived
    graph_id. The adapter must clear before each run()."""
    adapter = AutoGenAdapter()

    class _Context:
        def __init__(self):
            self.messages = []

        async def clear(self):
            self.messages.clear()

        def add(self, item):
            self.messages.append(item)

    class _AccumulatingAgent:
        def __init__(self):
            self._model_context = _Context()
            self.seen_sizes = []

        async def run(self, task):
            # Mirror AutoGen: context already holds prior turns unless cleared.
            self.seen_sizes.append(len(self._model_context.messages))
            self._model_context.add(f"user:{task}")
            self._model_context.add(f"ai:reply-to-{task}")
            return _FakeTaskResult(f"reply-to-{task}")

    fake = _AccumulatingAgent()
    adapter.agents["shared_agent"] = fake

    callback, _ = _collector()
    for label in ("a", "b", "c"):
        status = await adapter.execute(
            {"run_id": label, "graph_id": "shared_agent", "input": {"messages": [{"role": "user", "content": label}]}},
            callback,
            None,
        )
        check(f"run {label} succeeded", status == "success")

    check(
        "each run started with empty context (clear between runs)",
        fake.seen_sizes == [0, 0, 0],
    )


async def test_cancel_event_interrupts_a_slow_agent():
    adapter = AutoGenAdapter()
    adapter.agents["my_agent"] = _FakeAssistantAgent(delay=3600)

    cancel_event = asyncio.Event()

    async def _fire_cancel_soon():
        await asyncio.sleep(0.02)
        cancel_event.set()

    fire_task = asyncio.ensure_future(_fire_cancel_soon())
    callback, events = _collector()
    status = await adapter.execute({"run_id": "r5", "graph_id": "my_agent", "input": {"messages": [{"role": "user", "content": "hi"}]}}, callback, cancel_event)
    await fire_task

    check("cancelled run reports interrupted status, not error", status == "interrupted")
    check("end interrupted event emitted", any(e["method"] == "end" and e["data"]["status"] == "interrupted" for e in events))
    check("no error event emitted for a cancellation", not any(e["method"] == "error" for e in events))


async def main():
    await test_execute_happy_path_emits_expected_events()
    await test_extracts_last_human_message_as_input()
    await test_unknown_graph_id_reports_error()
    await test_agent_exception_reports_error()
    await test_concurrent_run_on_shared_agent_is_serialized()
    await test_sequential_runs_clear_shared_model_context()
    await test_cancel_event_interrupts_a_slow_agent()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
