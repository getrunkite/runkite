"""Self-check: LangChain OTel callbacks nest LLM/tool spans under runkite.run.

Usage:
    python/.venv/bin/python python/tests/test_otel_callbacks.py
"""

from __future__ import annotations

import os
import sys
from uuid import uuid4

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner import tracing  # noqa: E402
from runkite_runner.worker import build_run_config  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def _reset_tracing():
    tracing._tracer = None
    tracing._shutdown = None


def test_make_run_callbacks_empty_when_disabled():
    _reset_tracing()
    os.environ.pop("OTEL_EXPORTER_OTLP_ENDPOINT", None)
    os.environ.pop("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", None)
    check("no callbacks when tracing off", tracing.make_run_callbacks("r1") == [])


def test_build_run_config_merges_callbacks():
    try:
        from opentelemetry.sdk.trace import TracerProvider
    except ImportError:
        print("[SKIP] opentelemetry packages not installed")
        return
    try:
        import langchain_core  # noqa: F401
    except ImportError:
        print("[SKIP] langchain_core not installed")
        return

    _reset_tracing()
    provider = TracerProvider()
    # Use the provider's tracer directly -- avoid set_tracer_provider, which
    # can only be installed once per process and breaks later tests.
    tracing._tracer = provider.get_tracer("test")

    class Existing:
        pass

    existing = Existing()
    cfg = build_run_config(
        {
            "run_id": "r-merge",
            "thread_id": "t1",
            "graph_id": "g",
            "config": {"callbacks": [existing]},
        }
    )
    cbs = cfg.get("callbacks") or []
    check("existing callback preserved", existing in cbs)
    check("otel handler appended", len(cbs) == 2)
    check("second is otel handler", type(cbs[1]).__name__ == "OTelCallbackHandler")
    provider.shutdown()
    _reset_tracing()


def test_llm_and_tool_spans_under_run():
    try:
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
    except ImportError:
        print("[SKIP] opentelemetry packages not installed")
        return
    try:
        import langchain_core  # noqa: F401
    except ImportError:
        print("[SKIP] langchain_core not installed")
        return

    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    tracing._tracer = provider.get_tracer("test")

    llm_id = uuid4()
    tool_id = uuid4()

    with tracing.run_span("run-cb", graph_id="g") as span:
        check("run span active", span is not None)
        cbs = tracing.make_run_callbacks("run-cb")
        check("got one callback", len(cbs) == 1)
        h = cbs[0]
        h.on_chat_model_start(
            {"name": "fake-chat"},
            [[]],
            run_id=llm_id,
            metadata={"ls_model_name": "gpt-test"},
        )
        h.on_tool_start(
            {"name": "search"},
            "{}",
            run_id=tool_id,
            parent_run_id=llm_id,
        )
        h.on_tool_end("ok", run_id=tool_id)
        h.on_llm_end(None, run_id=llm_id)
        tracing.set_run_status(span, "success")

    spans = {s.name: s for s in exporter.get_finished_spans()}
    check("has runkite.run", "runkite.run" in spans)
    check("has runkite.llm", "runkite.llm" in spans)
    check("has runkite.tool", "runkite.tool" in spans)

    run_s = spans["runkite.run"]
    llm_s = spans["runkite.llm"]
    tool_s = spans["runkite.tool"]
    check("llm under run", llm_s.parent.span_id == run_s.context.span_id)
    check("tool under llm", tool_s.parent.span_id == llm_s.context.span_id)
    check("same trace", llm_s.context.trace_id == run_s.context.trace_id)
    check("llm.name attr", llm_s.attributes.get("llm.name") == "gpt-test")
    check("tool.name attr", tool_s.attributes.get("tool.name") == "search")
    check("run.id on llm", llm_s.attributes.get("run.id") == "run-cb")

    provider.shutdown()
    _reset_tracing()


def main():
    test_make_run_callbacks_empty_when_disabled()
    test_build_run_config_merges_callbacks()
    test_llm_and_tool_spans_under_run()
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
