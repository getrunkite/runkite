"""Self-check for the plain-LangChain adapter (python/adapters/langchain_adapter).

Proves:
1. load_config loads a Runnable from a langgraph.json-shaped config,
   same convention as the LangGraph runner's own config loading.
2. execute() extracts the last human message's text, invokes the
   Runnable with {"input": text}, and emits the LangGraph-runner-
   compatible RunEvent sequence (lifecycle -> values -> end).
3. A Runnable ending in a chat model (no output parser -- returns a
   BaseMessage-like object) is normalized to plain text the same way a
   string-returning chain is.
4. An unknown graph_id and a Runnable that raises both produce a
   well-formed "error" status instead of crashing the worker loop.

Usage:
    python/.venv/bin/python python/tests/test_langchain_adapter.py
"""

import asyncio
import json
import os
import sys
import tempfile

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "adapters", "langchain_adapter"))

from adapter import LangChainAdapter  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def _collector():
    """Returns (async_callback, events_list) -- event_callback in the
    real worker loop is always async, so tests need an async collector
    too, not a plain list.append."""
    events: list[dict] = []

    async def callback(event: dict):
        events.append(event)

    return callback, events


class _FakeRunnable:
    """Stands in for a real LangChain Runnable -- only .ainvoke matters
    to the adapter."""

    def __init__(self, response, raise_error=False, delay=0):
        self._response = response
        self._raise_error = raise_error
        self._delay = delay
        self.last_input = None

    async def ainvoke(self, input_dict):
        self.last_input = input_dict
        if self._delay:
            await asyncio.sleep(self._delay)
        if self._raise_error:
            raise RuntimeError("chain blew up")
        return self._response


class _FakeMessage:
    """Stands in for a BaseMessage (chat model with no output parser)."""

    def __init__(self, content):
        self.content = content


def _write_config(tmpdir, chain_module_code: str) -> str:
    chain_path = os.path.join(tmpdir, "chain.py")
    with open(chain_path, "w") as f:
        f.write(chain_module_code)
    config = {"graphs": {"my_chain": "./chain.py:chain"}, "dependencies": ["."]}
    config_path = os.path.join(tmpdir, "langgraph.json")
    with open(config_path, "w") as f:
        json.dump(config, f)
    return config_path


async def test_load_config_and_string_output():
    with tempfile.TemporaryDirectory() as tmpdir:
        config_path = _write_config(tmpdir, "class _R:\n    async def ainvoke(self, d):\n        return 'plain string reply'\nchain = _R()\n")
        adapter = LangChainAdapter()
        await adapter.load_config(config_path)

        check("graph loaded under its config key", "my_chain" in adapter.runnables)

        callback, events = _collector()
        status = await adapter.execute(
            {"run_id": "r1", "graph_id": "my_chain", "input": {"messages": [{"role": "user", "content": "hi there"}]}},
            callback,
            None,
        )
        check("status success", status == "success")
        methods = [e["method"] for e in events]
        check("lifecycle running emitted first", methods[0] == "lifecycle" and events[0]["data"]["event"] == "running")
        check("values emitted with reply appended", methods[1] == "values")
        check("end success emitted last", methods[-1] == "end" and events[-1]["data"]["status"] == "success")

        values_event = next(e for e in events if e["method"] == "values")
        check("output messages include the reply", values_event["data"]["messages"][-1] == {"role": "ai", "content": "plain string reply"})
        check("original input message preserved", values_event["data"]["messages"][0]["content"] == "hi there")


async def test_extracts_last_human_message_and_passes_input_key():
    adapter = LangChainAdapter()
    fake = _FakeRunnable("some reply")
    adapter.runnables["chat"] = fake

    callback, events = _collector()
    await adapter.execute(
        {"run_id": "r2", "graph_id": "chat", "input": {"messages": [
            {"role": "ai", "content": "earlier reply, must be ignored"},
            {"role": "user", "content": "the real question"},
        ]}},
        callback,
        None,
    )
    check("runnable invoked with {'input': <last human text>}", fake.last_input == {"input": "the real question"})


async def test_basemessage_output_normalized_to_text():
    adapter = LangChainAdapter()
    adapter.runnables["chat"] = _FakeRunnable(_FakeMessage("reply via .content"))

    callback, events = _collector()
    await adapter.execute({"run_id": "r3", "graph_id": "chat", "input": {"messages": [{"role": "user", "content": "hi"}]}}, callback, None)

    values_event = next(e for e in events if e["method"] == "values")
    check("BaseMessage-like result normalized via .content", values_event["data"]["messages"][-1]["content"] == "reply via .content")


async def test_unknown_graph_id_reports_error_without_crashing():
    adapter = LangChainAdapter()
    callback, events = _collector()
    status = await adapter.execute({"run_id": "r4", "graph_id": "does-not-exist", "input": {}}, callback, None)
    check("unknown graph_id status is error", status == "error")
    check("error event emitted", any(e["method"] == "error" for e in events))


async def test_runnable_exception_reports_error_without_crashing():
    adapter = LangChainAdapter()
    adapter.runnables["chat"] = _FakeRunnable(None, raise_error=True)
    callback, events = _collector()
    status = await adapter.execute({"run_id": "r5", "graph_id": "chat", "input": {"messages": [{"role": "user", "content": "hi"}]}}, callback, None)
    check("exception inside chain reports error status", status == "error")
    check("error event emitted with the exception message", any(e["method"] == "error" and "chain blew up" in e["data"]["message"] for e in events))


async def test_cancel_event_interrupts_a_slow_chain():
    adapter = LangChainAdapter()
    adapter.runnables["chat"] = _FakeRunnable("should never be returned", delay=3600)

    cancel_event = asyncio.Event()

    async def _fire_cancel_soon():
        await asyncio.sleep(0.02)
        cancel_event.set()

    fire_task = asyncio.ensure_future(_fire_cancel_soon())
    callback, events = _collector()
    status = await adapter.execute({"run_id": "r6", "graph_id": "chat", "input": {"messages": [{"role": "user", "content": "hi"}]}}, callback, cancel_event)
    await fire_task

    check("cancelled run reports interrupted status, not error", status == "interrupted")
    check("end interrupted event emitted", any(e["method"] == "end" and e["data"]["status"] == "interrupted" for e in events))
    check("no error event emitted for a cancellation", not any(e["method"] == "error" for e in events))


async def main():
    await test_load_config_and_string_output()
    await test_extracts_last_human_message_and_passes_input_key()
    await test_basemessage_output_normalized_to_text()
    await test_unknown_graph_id_reports_error_without_crashing()
    await test_runnable_exception_reports_error_without_crashing()
    await test_cancel_event_interrupts_a_slow_chain()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
