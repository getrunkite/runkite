"""Self-check: CrewAI OTel listeners nest LLM/tool spans under runkite.run.

Usage (adapter venv):
    PYTHONPATH=python:python/adapters \\
      python/adapters/crewai_adapter/.venv/bin/python \\
      python/adapters/crewai_adapter/test_otel_events.py
"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", ".."))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner import tracing  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def test_noop_when_tracing_off():
    tracing._tracer = None
    from crewai_adapter.otel_events import attach_otel_listeners

    with attach_otel_listeners("r1"):
        pass
    check("attach is a no-op when tracing off", True)


def test_synthetic_events_under_run_span():
    try:
        from crewai.events import crewai_event_bus
        from crewai.events.types.llm_events import LLMCallCompletedEvent, LLMCallStartedEvent, LLMCallType
        from crewai.events.types.tool_usage_events import ToolUsageFinishedEvent, ToolUsageStartedEvent
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
    except ImportError as e:
        print(f"[SKIP] missing dep: {e}")
        return

    from crewai_adapter.otel_events import attach_otel_listeners

    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    tracing._tracer = provider.get_tracer("test")

    with tracing.run_span("run-crew") as span:
        check("run span active", span is not None)
        with attach_otel_listeners("run-crew"):
            started = LLMCallStartedEvent(model="gpt-test", call_id="c1", messages=[])
            crewai_event_bus.emit(None, started)
            crewai_event_bus.emit(
                None,
                LLMCallCompletedEvent(
                    messages=[],
                    response="ok",
                    call_type=LLMCallType.LLM_CALL,
                    call_id="c1",
                    started_event_id=started.event_id,
                ),
            )
            tool_start = ToolUsageStartedEvent(tool_name="search", tool_args={})
            crewai_event_bus.emit(None, tool_start)
            crewai_event_bus.emit(
                None,
                ToolUsageFinishedEvent(
                    tool_name="search",
                    tool_args={},
                    started_at=tool_start.timestamp,
                    finished_at=tool_start.timestamp,
                    from_cache=False,
                    output="ok",
                    started_event_id=tool_start.event_id,
                ),
            )
            crewai_event_bus.flush(timeout=5.0)

    spans = {s.name: s for s in exporter.get_finished_spans()}
    check("has runkite.run", "runkite.run" in spans)
    check("has runkite.llm", "runkite.llm" in spans)
    check("has runkite.tool", "runkite.tool" in spans)
    check("llm under run", spans["runkite.llm"].parent.span_id == spans["runkite.run"].context.span_id)
    check("llm.name", spans["runkite.llm"].attributes.get("llm.name") == "gpt-test")
    check("tool.name", spans["runkite.tool"].attributes.get("tool.name") == "search")

    provider.shutdown()
    tracing._tracer = None


def test_concurrent_attaches_do_not_cross_attribute():
    """Two open attach windows must not duplicate one emit onto both runs.

    Reproduces the pre-fix bug: N per-call closures on the global bus
    meant every LLM event created a span under every concurrently attached
    run. With ContextVar-scoped state, only the emitting job's parent gets
    the child span.
    """
    import threading
    import time

    try:
        from crewai.events import crewai_event_bus
        from crewai.events.types.llm_events import LLMCallCompletedEvent, LLMCallStartedEvent, LLMCallType
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
    except ImportError as e:
        print(f"[SKIP] missing dep: {e}")
        return

    from crewai_adapter.otel_events import attach_otel_listeners

    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    tracing._tracer = provider.get_tracer("test")

    ready_a = threading.Event()
    release_a = threading.Event()
    errors: list[BaseException] = []

    def hold_run_a():
        try:
            with tracing.run_span("run-A"):
                with attach_otel_listeners("run-A"):
                    ready_a.set()
                    release_a.wait(timeout=5.0)
        except BaseException as e:
            errors.append(e)

    t = threading.Thread(target=hold_run_a, daemon=True)
    t.start()
    check("run-A attach ready", ready_a.wait(timeout=5.0))

    with tracing.run_span("run-B"):
        with attach_otel_listeners("run-B"):
            # Brief overlap so both attaches are live when we emit.
            time.sleep(0.05)
            started = LLMCallStartedEvent(model="gpt-b-only", call_id="only-b", messages=[])
            crewai_event_bus.emit(None, started)
            crewai_event_bus.emit(
                None,
                LLMCallCompletedEvent(
                    messages=[],
                    response="ok",
                    call_type=LLMCallType.LLM_CALL,
                    call_id="only-b",
                    started_event_id=started.event_id,
                ),
            )
            crewai_event_bus.flush(timeout=5.0)

    release_a.set()
    t.join(timeout=5.0)
    check("run-A thread clean", not errors)

    finished = list(exporter.get_finished_spans())
    llm = [s for s in finished if s.name == "runkite.llm"]
    runs = {s.attributes.get("run.id"): s for s in finished if s.name == "runkite.run"}
    check("exactly one llm span", len(llm) == 1)
    check("llm attributed to run-B", llm[0].attributes.get("run.id") == "run-B")
    check("llm under run-B", llm[0].parent.span_id == runs["run-B"].context.span_id)
    check("llm.name", llm[0].attributes.get("llm.name") == "gpt-b-only")

    provider.shutdown()
    tracing._tracer = None


def main():
    test_noop_when_tracing_off()
    test_synthetic_events_under_run_span()
    test_concurrent_attaches_do_not_cross_attribute()
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
