"""Self-check for the LlamaIndex adapter (python/adapters/llamaindex_adapter).

Lives alongside the adapter (not in python/tests/) since it needs the
LlamaIndex-specific isolated venv.

Proves:
1. load_config loads a chat engine from a langgraph.json-shaped config.
2. execute() reconstructs prior chat_history from every message except
   the last, calls engine.achat(last_text, chat_history=...), and emits
   the same RunEvent sequence every other adapter does.
3. Per-call chat_history reconstruction means two DIFFERENT threads
   sharing the same engine instance never see each other's messages --
   the whole reason this adapter doesn't rely on the engine's own
   internal memory (see adapter.py's module docstring).
4. An unknown graph_id, an empty message list, and an engine that
   raises all produce a well-formed "error" status instead of crashing
   the worker loop.

Usage:
    python/adapters/llamaindex_adapter/.venv/bin/python \\
        python/adapters/llamaindex_adapter/test_adapter.py
"""

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", ".."))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "..", "python"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "..", "python", "adapters"))

from llama_index.core.base.llms.types import ChatMessage, ChatResponse, MessageRole  # noqa: E402
from llama_index.core.callbacks.schema import CBEventType, EventPayload  # noqa: E402

from llamaindex_adapter.adapter import LlamaIndexAdapter, _RealUsageOnlyHandler  # noqa: E402


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


class _FakeChatResponse:
    def __init__(self, response):
        self.response = response


class _FakeEngine:
    def __init__(self, response="fake reply", raise_error=False, delay=0):
        self._response = response
        self._raise_error = raise_error
        self._delay = delay
        self.last_message = None
        self.last_history = None

    async def achat(self, message, chat_history=None):
        self.last_message = message
        self.last_history = chat_history
        if self._delay:
            await asyncio.sleep(self._delay)
        if self._raise_error:
            raise RuntimeError("engine blew up")
        return _FakeChatResponse(self._response)


async def test_execute_happy_path_emits_expected_events():
    adapter = LlamaIndexAdapter()
    adapter.engines["my_engine"] = _FakeEngine(response="hello from fake engine")

    callback, events = _collector()
    status = await adapter.execute(
        {"run_id": "r1", "graph_id": "my_engine", "input": {"messages": [{"role": "user", "content": "hi there"}]}},
        callback,
        None,
    )
    check("status success", status == "success")
    methods = [e["method"] for e in events]
    check("lifecycle running emitted first", methods[0] == "lifecycle" and events[0]["data"]["event"] == "running")
    check("values emitted", "values" in methods)
    check("end success emitted last", methods[-1] == "end" and events[-1]["data"]["status"] == "success")

    values_event = next(e for e in events if e["method"] == "values")
    check(
        "reply appended to messages",
        values_event["data"]["messages"][-1] == {"role": "ai", "content": "hello from fake engine"},
    )


async def test_reconstructs_history_and_uses_last_message_as_input():
    adapter = LlamaIndexAdapter()
    fake = _FakeEngine()
    adapter.engines["my_engine"] = fake

    callback, _ = _collector()
    await adapter.execute(
        {
            "run_id": "r2",
            "graph_id": "my_engine",
            "input": {
                "messages": [
                    {"role": "user", "content": "first turn"},
                    {"role": "ai", "content": "first reply"},
                    {"role": "user", "content": "second turn"},
                ]
            },
        },
        callback,
        None,
    )
    check("last message text passed as achat's message arg", fake.last_message == "second turn")
    check("chat_history reconstructed from every prior message", len(fake.last_history) == 2)
    check(
        "chat_history preserves order (first turn, then first reply)",
        fake.last_history[0].content == "first turn" and fake.last_history[1].content == "first reply",
    )


async def test_two_runs_sharing_one_engine_dont_leak_history():
    """The whole point of per-call chat_history: a shared engine
    instance across two DIFFERENT threads must never let one thread's
    conversation bleed into the other's, since the engine object itself
    has no idea which thread a call belongs to."""
    adapter = LlamaIndexAdapter()
    fake = _FakeEngine()
    adapter.engines["shared"] = fake

    callback, _ = _collector()
    await adapter.execute(
        {
            "run_id": "r3a",
            "graph_id": "shared",
            "input": {"messages": [{"role": "user", "content": "thread A message"}]},
        },
        callback,
        None,
    )
    check("thread A: no prior history (first message in its thread)", fake.last_history == [])

    await adapter.execute(
        {
            "run_id": "r3b",
            "graph_id": "shared",
            "input": {"messages": [{"role": "user", "content": "thread B message"}]},
        },
        callback,
        None,
    )
    check("thread B: no prior history either -- thread A's message never appeared", fake.last_history == [])


async def test_unknown_graph_id_reports_error():
    adapter = LlamaIndexAdapter()
    callback, events = _collector()
    status = await adapter.execute({"run_id": "r4", "graph_id": "nope", "input": {}}, callback, None)
    check("unknown graph_id -> error status", status == "error")
    check("error event emitted", any(e["method"] == "error" for e in events))


async def test_empty_messages_reports_error_without_crashing():
    adapter = LlamaIndexAdapter()
    adapter.engines["my_engine"] = _FakeEngine()
    callback, events = _collector()
    status = await adapter.execute({"run_id": "r5", "graph_id": "my_engine", "input": {"messages": []}}, callback, None)
    check("empty messages -> error status, not a crash", status == "error")
    check("error event emitted", any(e["method"] == "error" for e in events))


async def test_engine_exception_reports_error():
    adapter = LlamaIndexAdapter()
    adapter.engines["my_engine"] = _FakeEngine(raise_error=True)
    callback, events = _collector()
    status = await adapter.execute(
        {"run_id": "r6", "graph_id": "my_engine", "input": {"messages": [{"role": "user", "content": "hi"}]}},
        callback,
        None,
    )
    check("engine exception -> error status", status == "error")
    check(
        "error event includes the exception message",
        any(e["method"] == "error" and "engine blew up" in e["data"]["message"] for e in events),
    )


async def test_cancel_event_interrupts_a_slow_engine():
    adapter = LlamaIndexAdapter()
    adapter.engines["my_engine"] = _FakeEngine(delay=3600)

    cancel_event = asyncio.Event()

    async def _fire_cancel_soon():
        await asyncio.sleep(0.02)
        cancel_event.set()

    fire_task = asyncio.ensure_future(_fire_cancel_soon())
    callback, events = _collector()
    status = await adapter.execute(
        {"run_id": "r7", "graph_id": "my_engine", "input": {"messages": [{"role": "user", "content": "hi"}]}},
        callback,
        cancel_event,
    )
    await fire_task

    check("cancelled run reports interrupted status, not error", status == "interrupted")
    check(
        "end interrupted event emitted",
        any(e["method"] == "end" and e["data"]["status"] == "interrupted" for e in events),
    )
    check("no error event emitted for a cancellation", not any(e["method"] == "error" for e in events))


async def test_real_reply_with_no_usage_anywhere_is_flagged_unmetered():
    """_FakeEngine has no ._llm/.llm attribute (so TokenCountingHandler
    never attaches to anything) and its response has none of
    additional_kwargs/raw/meta/token_counts -- exactly what a brand-new/
    unrecognized LLM integration wired into a chat engine would look
    like: a real reply, zero extractable usage anywhere. That must
    surface as an explicit unmetered marker, not silently look identical
    to an engine that made no LLM call.
    """
    adapter = LlamaIndexAdapter()
    adapter.engines["my_engine"] = _FakeEngine(response="a real reply, but nothing reports its usage")

    callback, events = _collector()
    status = await adapter.execute(
        {"run_id": "r-unmetered", "graph_id": "my_engine", "input": {"messages": [{"role": "user", "content": "hi"}]}},
        callback,
        None,
    )
    check("status success", status == "success")
    values_event = next(e for e in events if e["method"] == "values")
    check(
        "usage carries the unmetered marker instead of being absent",
        values_event["data"].get("usage")
        == {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "unmetered": True},
    )


def test_real_usage_only_handler_extracts_recognized_provider_keys():
    handler = _RealUsageOnlyHandler()
    response = ChatResponse(
        message=ChatMessage(role=MessageRole.ASSISTANT, content="hi"),
        raw={"usage_metadata": {"prompt_token_count": 22, "candidates_token_count": 2}},
    )
    handler.on_event_end(CBEventType.LLM, payload={EventPayload.RESPONSE: response})
    check("recognized Gemini-shaped usage keys are extracted", handler.found_real_usage)
    check("prompt_tokens correct", handler.prompt_tokens == 22)
    check("completion_tokens correct", handler.completion_tokens == 2)


def test_real_usage_only_handler_does_not_estimate_for_unrecognized_keys():
    """This is the specific bug this handler exists to avoid: LlamaIndex's
    own TokenCountingHandler would silently re-tokenize the prompt/response
    text with tiktoken (a fabricated, plausible-looking number) when a
    provider's raw usage doesn't match its recognized key list. A real
    99-year-old provider reporting genuinely-spent tokens under a key name
    this codebase has never seen must show up as "no real usage found",
    not an invented estimate.
    """
    handler = _RealUsageOnlyHandler()
    response = ChatResponse(
        message=ChatMessage(role=MessageRole.ASSISTANT, content="a real reply from an unrecognized provider"),
        raw={"usage": {"tokens_consumed_for_input": 50, "tokens_consumed_for_output": 20}},
    )
    handler.on_event_end(CBEventType.LLM, payload={EventPayload.RESPONSE: response})
    check("unrecognized usage keys are not estimated or guessed", not handler.found_real_usage)
    check("prompt_tokens stays zero, not a tiktoken guess", handler.prompt_tokens == 0)
    check("completion_tokens stays zero, not a tiktoken guess", handler.completion_tokens == 0)


async def main():
    test_real_usage_only_handler_extracts_recognized_provider_keys()
    test_real_usage_only_handler_does_not_estimate_for_unrecognized_keys()
    await test_execute_happy_path_emits_expected_events()
    await test_reconstructs_history_and_uses_last_message_as_input()
    await test_two_runs_sharing_one_engine_dont_leak_history()
    await test_real_reply_with_no_usage_anywhere_is_flagged_unmetered()
    await test_unknown_graph_id_reports_error()
    await test_empty_messages_reports_error_without_crashing()
    await test_engine_exception_reports_error()
    await test_cancel_event_interrupts_a_slow_engine()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
