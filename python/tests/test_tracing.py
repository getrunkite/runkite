"""Self-check for runner tracing.py: no-op without OTEL endpoint;
child span under a parent traceparent when an in-memory provider is installed.

Usage:
    python/.venv/bin/python python/tests/test_tracing.py
"""

from __future__ import annotations

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner import tracing  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def test_noop_without_endpoint():
    os.environ.pop("OTEL_EXPORTER_OTLP_ENDPOINT", None)
    os.environ.pop("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", None)
    # Reset module state so a prior test's init does not stick.
    tracing._tracer = None
    tracing._shutdown = None
    shutdown = tracing.init()
    shutdown()
    with tracing.run_span("r1", graph_id="g", trace_context={"traceparent": "00-" + "a" * 32 + "-" + "b" * 16 + "-01"}) as span:
        check("run_span yields None when tracing disabled", span is None)


def test_span_under_parent_traceparent():
    try:
        from opentelemetry import trace
        from opentelemetry.propagate import set_global_textmap
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import SimpleSpanProcessor
        from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
        from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
    except ImportError:
        print("[SKIP] opentelemetry packages not installed")
        return

    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    trace.set_tracer_provider(provider)
    set_global_textmap(TraceContextTextMapPropagator())
    tracing._tracer = trace.get_tracer("test")
    tracing._shutdown = lambda: provider.shutdown()

    # Real W3C parent: 32-hex trace id, 16-hex span id.
    parent_tp = "00-" + ("11" * 16) + "-" + ("22" * 8) + "-01"
    with tracing.run_span(
        "run-otel",
        graph_id="echo",
        thread_id="t1",
        tenant_id="acme",
        trace_context={"traceparent": parent_tp, "correlation_id": "corr-1"},
    ) as span:
        check("run_span yields a live span", span is not None)
        tracing.set_run_status(span, "success")

    spans = exporter.get_finished_spans()
    check("exactly one finished span", len(spans) == 1)
    s = spans[0]
    check("span name is runkite.run", s.name == "runkite.run")
    check("run.id attribute", s.attributes.get("run.id") == "run-otel")
    check("nested under parent trace id", format(s.context.trace_id, "032x") == "11" * 16)
    check("parent span id matches carrier", format(s.parent.span_id, "016x") == "22" * 8)
    check("run.status recorded", s.attributes.get("run.status") == "success")
    provider.shutdown()


def main():
    test_noop_without_endpoint()
    test_span_under_parent_traceparent()
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
