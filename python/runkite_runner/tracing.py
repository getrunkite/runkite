"""Runner-side OpenTelemetry: nest each job under the control plane's
W3C traceparent from RunAssignment.trace_context.

Disabled by default (zero overhead) until OTEL_EXPORTER_OTLP_ENDPOINT
or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set -- same contract as the
control plane's internal/tracing package. Soft-imports the OTel SDK so
a venv without those packages still runs; if an endpoint is configured
but the packages are missing, we log once and stay no-op.
"""

from __future__ import annotations

import logging
import os
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from typing import Any

logger = logging.getLogger(__name__)

_tracer: Any = None
_shutdown: Callable[[], None] | None = None
_warned_missing = False


def _endpoint_configured() -> bool:
    return bool(os.environ.get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") or os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT"))


def init() -> Callable[[], None]:
    """Install a global TracerProvider from OTEL_* env vars.

    Always returns a shutdown callable (safe to call / defer even when
    tracing was never enabled).
    """
    global _tracer, _shutdown, _warned_missing

    def noop() -> None:
        return None

    if not _endpoint_configured():
        return noop
    if _shutdown is not None:
        return _shutdown

    try:
        from opentelemetry import trace
        from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter as GrpcExporter
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter as HttpExporter
        from opentelemetry.propagate import set_global_textmap
        from opentelemetry.sdk.resources import Resource
        from opentelemetry.sdk.trace import TracerProvider
        from opentelemetry.sdk.trace.export import BatchSpanProcessor
        from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
    except ImportError:
        if not _warned_missing:
            logger.warning(
                "OTEL_EXPORTER_OTLP_* is set but opentelemetry packages are not installed; "
                "runner tracing disabled. pip install 'runkite-runner[otel]'"
            )
            _warned_missing = True
        return noop

    protocol = (
        os.environ.get("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL") or os.environ.get("OTEL_EXPORTER_OTLP_PROTOCOL") or "grpc"
    ).lower()
    exporter = HttpExporter() if protocol.startswith("http") else GrpcExporter()

    service_name = os.environ.get("OTEL_SERVICE_NAME") or "runkite-runner"
    provider = TracerProvider(resource=Resource.create({"service.name": service_name}))
    provider.add_span_processor(BatchSpanProcessor(exporter))
    trace.set_tracer_provider(provider)
    # Make extract()/inject() use W3C even if another lib changed the global.
    set_global_textmap(TraceContextTextMapPropagator())

    _tracer = trace.get_tracer("runkite.runner")

    def _do_shutdown() -> None:
        provider.shutdown()

    _shutdown = _do_shutdown
    logger.info("OpenTelemetry tracing enabled (protocol=%s service=%s)", protocol, service_name)
    return _shutdown


@contextmanager
def run_span(
    run_id: str,
    *,
    graph_id: str = "",
    thread_id: str = "",
    tenant_id: str = "",
    trace_context: dict | None = None,
) -> Iterator[Any]:
    """Activate the CP parent context and open a child span for this job.

    Yields the span (or None when tracing is disabled / unavailable).
    """
    if _tracer is None:
        yield None
        return

    from opentelemetry.propagate import extract
    from opentelemetry.trace import Status, StatusCode

    tc = trace_context or {}
    carrier: dict[str, str] = {}
    if tp := tc.get("traceparent"):
        carrier["traceparent"] = tp
    if ts := tc.get("tracestate"):
        carrier["tracestate"] = ts
    parent_ctx = extract(carrier) if carrier else None

    with _tracer.start_as_current_span("runkite.run", context=parent_ctx) as span:
        span.set_attribute("run.id", run_id)
        if graph_id:
            span.set_attribute("graph.id", graph_id)
        if thread_id:
            span.set_attribute("thread.id", thread_id)
        if tenant_id:
            span.set_attribute("tenant.id", tenant_id)
        if cid := tc.get("correlation_id"):
            span.set_attribute("correlation.id", cid)
        try:
            yield span
        except Exception as exc:
            span.set_status(Status(StatusCode.ERROR, str(exc)))
            span.record_exception(exc)
            raise


def set_run_status(span: Any, status: str) -> None:
    """Record the job's terminal run status on the span (when present)."""
    if span is None:
        return
    from opentelemetry.trace import Status, StatusCode

    span.set_attribute("run.status", status)
    if status == "error":
        span.set_status(Status(StatusCode.ERROR))
    else:
        span.set_status(Status(StatusCode.OK))


def get_tracer() -> Any | None:
    """Return the installed tracer, or None when tracing is disabled."""
    return _tracer


def make_run_callbacks(run_id: str = "") -> list:
    """LangChain handlers that open LLM/tool child spans under runkite.run.

    Empty when tracing is disabled or langchain_core is unavailable.
    Call inside an active run_span (or after extract) so the captured
    parent context is the job span, not the process root.
    """
    if _tracer is None:
        return []
    try:
        from opentelemetry import context as otel_context

        from .otel_callbacks import new_handler
    except ImportError:
        return []
    handler = new_handler(_tracer, otel_context.get_current(), run_id=run_id)
    if handler is None:
        return []
    return [handler]
