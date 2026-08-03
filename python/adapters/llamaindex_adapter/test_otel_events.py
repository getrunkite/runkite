"""Self-check: LlamaIndex OTel handler nests LLM spans under runkite.run.

Usage (adapter venv):
    PYTHONPATH=python:python/adapters \\
      python/adapters/llamaindex_adapter/.venv/bin/python \\
      python/adapters/llamaindex_adapter/test_otel_events.py
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
    from llamaindex_adapter.otel_events import attach_otel_job

    with attach_otel_job("r1"):
        pass
    check("attach is a no-op when tracing off", True)


def test_synthetic_llm_events_under_run_span():
    try:
        from llama_index.core.instrumentation import get_dispatcher
        from llama_index.core.instrumentation.events.llm import LLMChatEndEvent, LLMChatStartEvent
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
    except ImportError as e:
        print(f"[SKIP] missing dep: {e}")
        return

    from llamaindex_adapter.otel_events import attach_otel_job

    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    tracing._tracer = provider.get_tracer("test")

    with tracing.run_span("run-li") as span:
        check("run span active", span is not None)
        with attach_otel_job("run-li"):
            start = LLMChatStartEvent(messages=[], additional_kwargs={}, model_dict={"model": "gpt-test"})
            get_dispatcher().event(start)
            get_dispatcher().event(LLMChatEndEvent(messages=[], response=None, span_id=start.span_id))

    spans = {s.name: s for s in exporter.get_finished_spans()}
    check("has runkite.run", "runkite.run" in spans)
    check("has runkite.llm", "runkite.llm" in spans)
    check("llm under run", spans["runkite.llm"].parent.span_id == spans["runkite.run"].context.span_id)
    check("llm.name", spans["runkite.llm"].attributes.get("llm.name") == "gpt-test")

    provider.shutdown()
    tracing._tracer = None


def main():
    test_noop_when_tracing_off()
    test_synthetic_llm_events_under_run_span()
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
