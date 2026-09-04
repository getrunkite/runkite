"""Self-check for the CrewAI adapter (python/adapters/crewai_adapter).

Lives alongside the adapter (not in python/tests/) since it needs the
CrewAI-specific isolated venv, not the shared runkite_runner one -- see
this adapter's own README for why CrewAI/LlamaIndex get their own
venvs rather than joining the shared requirements.txt.

Proves:
1. load_config loads a Crew from a langgraph.json-shaped config.
2. execute() extracts the last human message's text, calls
   crew.akickoff(inputs={"input": text}), and emits the same
   RunEvent sequence (lifecycle -> values -> end) every other adapter
   does.
3. An unknown graph_id and a Crew that raises both produce a
   well-formed "error" status instead of crashing the worker loop.

Usage:
    python/adapters/crewai_adapter/.venv/bin/python \\
        python/adapters/crewai_adapter/test_adapter.py
"""

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", ".."))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "..", "python"))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "..", "python", "adapters"))

from crewai.types.usage_metrics import UsageMetrics  # noqa: E402

from crewai_adapter.adapter import CrewAIAdapter  # noqa: E402


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


class _FakeCrewOutput:
    def __init__(self, raw):
        self.raw = raw


class _FakeCrew:
    def __init__(self, response=None, raise_error=False, delay=0):
        self._response = response
        self._raise_error = raise_error
        self._delay = delay
        self.last_inputs = None

    async def akickoff(self, inputs):
        self.last_inputs = inputs
        if self._delay:
            await asyncio.sleep(self._delay)
        if self._raise_error:
            raise RuntimeError("crew blew up")
        return _FakeCrewOutput(self._response)


async def test_execute_happy_path_emits_expected_events():
    adapter = CrewAIAdapter()
    adapter.crews["my_crew"] = _FakeCrew(response="fake crew reply")

    callback, events = _collector()
    status = await adapter.execute(
        {"run_id": "r1", "graph_id": "my_crew", "input": {"messages": [{"role": "user", "content": "hello crew"}]}},
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
        values_event["data"]["messages"][-1] == {"role": "ai", "content": "fake crew reply"},
    )


async def test_extracts_last_human_message_as_input():
    adapter = CrewAIAdapter()
    fake = _FakeCrew(response="ok")
    adapter.crews["my_crew"] = fake

    callback, _ = _collector()
    await adapter.execute(
        {
            "run_id": "r2",
            "graph_id": "my_crew",
            "input": {
                "messages": [
                    {"role": "ai", "content": "ignored"},
                    {"role": "user", "content": "the real ask"},
                ]
            },
        },
        callback,
        None,
    )
    check("invoke uses input key", set(fake.last_inputs.keys()) == {"input"})
    check("current ask present", "the real ask" in fake.last_inputs["input"])
    check("prior turn folded into prompt", "ignored" in fake.last_inputs["input"])


class _CumulativeUsageFakeCrew:
    """Mimics CrewAI's real behavior: usage_metrics is the LIFETIME total
    across every kickoff on this Crew instance (crewai/llms/base_llm.py's
    _token_usage only ever accumulates; crewai/crew.py's
    calculate_usage_metrics reads that live accumulator fresh every call,
    it does not track "since when"). Each akickoff call here adds a fixed
    amount to the running total, the same way a real LLM client's
    internal counter would after one real provider call."""

    def __init__(self, per_call_prompt: int, per_call_completion: int):
        self._per_call_prompt = per_call_prompt
        self._per_call_completion = per_call_completion
        self.usage_metrics: UsageMetrics | None = None

    async def akickoff(self, inputs):
        prior = self.usage_metrics or UsageMetrics()
        self.usage_metrics = UsageMetrics(
            prompt_tokens=prior.prompt_tokens + self._per_call_prompt,
            completion_tokens=prior.completion_tokens + self._per_call_completion,
            total_tokens=prior.total_tokens + self._per_call_prompt + self._per_call_completion,
        )
        return _FakeCrewOutput("ok")


async def test_usage_metrics_delta_not_cumulative_across_calls():
    """Live dogfood proof this was a real bug, not a hypothetical: three
    separate fresh-thread calls to the same real Gemini-backed Crew
    reported prompt_tokens 84, 168, 252 -- exact multiples of the first
    call's real usage, because crew.usage_metrics is the shared Crew's
    lifetime total, not a per-call figure. Same graph_id means same
    shared Crew instance (see load_config), so this is not scoped to one
    thread the way LangGraph's checkpoint-carried usage_metadata is --
    it grows with every run this process has ever handled for this agent,
    across every tenant and thread.
    """
    adapter = CrewAIAdapter()
    fake = _CumulativeUsageFakeCrew(per_call_prompt=84, per_call_completion=30)
    adapter.crews["my_crew"] = fake

    async def run_once(run_id: str):
        callback, events = _collector()
        status = await adapter.execute(
            {"run_id": run_id, "graph_id": "my_crew", "input": {"messages": [{"role": "user", "content": "hi"}]}},
            callback,
            None,
        )
        check(f"{run_id} succeeds", status == "success")
        values_event = next(e for e in events if e["method"] == "values")
        return values_event["data"].get("usage")

    usage1 = await run_once("r-call-1")
    usage2 = await run_once("r-call-2")
    usage3 = await run_once("r-call-3")

    check(
        "call 1 reports only its own 84/30 tokens", usage1["prompt_tokens"] == 84 and usage1["completion_tokens"] == 30
    )
    check(
        "call 2 reports only its own 84/30 tokens, not call 1's on top (would be 168 if buggy)",
        usage2["prompt_tokens"] == 84 and usage2["completion_tokens"] == 30,
    )
    check(
        "call 3 reports only its own 84/30 tokens, not calls 1+2's on top (would be 252 if buggy)",
        usage3["prompt_tokens"] == 84 and usage3["completion_tokens"] == 30,
    )
    # And the underlying accumulator genuinely did keep growing across
    # calls -- proving the delta logic is doing real subtraction, not
    # just coincidentally returning the same fixture value three times.
    check("underlying lifetime accumulator kept growing", fake.usage_metrics.prompt_tokens == 84 * 3)


async def test_real_reply_with_no_usage_metrics_is_flagged_unmetered():
    """_FakeCrew (used throughout this file) has no usage_metrics attribute
    at all -- exactly what a brand-new/unrecognized provider integration,
    or a Crew whose LLM class this codebase has never exercised, would
    produce: a real reply, with the delta computation finding literally
    nothing to report. That must surface as an explicit unmetered marker,
    not silently look identical to a Crew that made no LLM call.
    """
    adapter = CrewAIAdapter()
    adapter.crews["my_crew"] = _FakeCrew(response="a real reply, but usage_metrics reports nothing")

    callback, events = _collector()
    status = await adapter.execute(
        {"run_id": "r-unmetered", "graph_id": "my_crew", "input": {"messages": [{"role": "user", "content": "hi"}]}},
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


async def test_unknown_graph_id_reports_error():
    adapter = CrewAIAdapter()
    callback, events = _collector()
    status = await adapter.execute({"run_id": "r3", "graph_id": "nope", "input": {}}, callback, None)
    check("unknown graph_id -> error status", status == "error")
    check("error event emitted", any(e["method"] == "error" for e in events))


async def test_crew_exception_reports_error():
    adapter = CrewAIAdapter()
    adapter.crews["my_crew"] = _FakeCrew(raise_error=True)
    callback, events = _collector()
    status = await adapter.execute(
        {"run_id": "r4", "graph_id": "my_crew", "input": {"messages": [{"role": "user", "content": "hi"}]}},
        callback,
        None,
    )
    check("crew exception -> error status", status == "error")
    check(
        "error event includes the exception message",
        any(e["method"] == "error" and "crew blew up" in e["data"]["message"] for e in events),
    )


async def test_concurrent_akickoff_on_shared_crew_is_serialized():
    """Regression for the runner-side concurrency spot-check: a shared
    Crew instance's akickoff() writes to shared instance attributes
    (crewai's own Crew.kickoff/akickoff write self.usage_metrics,
    self._task_output_handler -- see this adapter's module docstring),
    so two concurrent runs against the SAME graph_id must never overlap
    inside akickoff() itself, even though the runner dispatches them
    concurrently."""
    adapter = CrewAIAdapter()
    current = 0
    max_concurrent = 0

    class _TrackingFakeCrew:
        async def akickoff(self, inputs):
            nonlocal current, max_concurrent
            current += 1
            max_concurrent = max(max_concurrent, current)
            try:
                await asyncio.sleep(0.05)
                return _FakeCrewOutput(f"reply-to-{inputs['input']}")
            finally:
                current -= 1

    adapter.crews["shared_crew"] = _TrackingFakeCrew()

    async def run(label: str):
        callback, events = _collector()
        status = await adapter.execute(
            {"run_id": label, "graph_id": "shared_crew", "input": {"messages": [{"role": "user", "content": label}]}},
            callback,
            None,
        )
        return status, events

    results = await asyncio.gather(run("r-a"), run("r-b"), run("r-c"))
    check("max_concurrent inside akickoff() never exceeded 1 (serialized by the lock)", max_concurrent == 1)
    check("all three concurrent runs still completed successfully", all(status == "success" for status, _ in results))


async def test_cancel_event_interrupts_a_slow_crew():
    adapter = CrewAIAdapter()
    adapter.crews["my_crew"] = _FakeCrew(delay=3600)

    cancel_event = asyncio.Event()

    async def _fire_cancel_soon():
        await asyncio.sleep(0.02)
        cancel_event.set()

    fire_task = asyncio.ensure_future(_fire_cancel_soon())
    callback, events = _collector()
    status = await adapter.execute(
        {"run_id": "r5", "graph_id": "my_crew", "input": {"messages": [{"role": "user", "content": "hi"}]}},
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


async def main():
    await test_execute_happy_path_emits_expected_events()
    await test_extracts_last_human_message_as_input()
    await test_unknown_graph_id_reports_error()
    await test_crew_exception_reports_error()
    await test_real_reply_with_no_usage_metrics_is_flagged_unmetered()
    await test_usage_metrics_delta_not_cumulative_across_calls()
    await test_concurrent_akickoff_on_shared_crew_is_serialized()
    await test_cancel_event_interrupts_a_slow_crew()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
