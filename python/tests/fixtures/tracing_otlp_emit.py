"""Emit three synthetic spans through the real Python runner tracing helpers.

Only the owned loopback Langfuse qualification harness should invoke this.
The receipt confirms local SDK completion, not durable backend ingestion.
"""

from __future__ import annotations

import json
import os
import re
import sys
from pathlib import Path
from urllib.parse import urlsplit
from uuid import UUID


def _isolate_http_credentials() -> None:
    # Requests can discover a user's home through pwd even without HOME, and
    # netrc authentication can override an explicit Authorization header.
    # This standalone synthetic fixture must use only its supplied OTLP auth.
    os.environ["NETRC"] = os.devnull


def main() -> None:
    if os.environ.get("RUNKITE_TEST_OTLP_EMIT") != "1":
        raise ValueError("explicit synthetic emitter mode is required")
    endpoint = urlsplit(os.environ.get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", ""))
    if (
        endpoint.scheme != "http"
        or endpoint.hostname != "127.0.0.1"
        or endpoint.port is None
        or endpoint.port <= 0
        or endpoint.username is not None
        or endpoint.password is not None
        or endpoint.query
        or endpoint.fragment
        or endpoint.path != "/api/public/otel/v1/traces"
    ):
        raise ValueError("a full owned loopback OTLP trace endpoint is required")
    protocol = os.environ.get("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL") or os.environ.get("OTEL_EXPORTER_OTLP_PROTOCOL")
    if protocol != "http/protobuf":
        raise ValueError("the Python emitter requires explicit http/protobuf")
    parent = os.environ.get("RUNKITE_TEST_OTLP_PARENT", "")
    if not re.fullmatch(r"00-(?!0{32})[0-9a-f]{32}-(?!0{16})[0-9a-f]{16}-01", parent):
        raise ValueError("a sampled, nonzero W3C parent is required")
    service = os.environ.get("OTEL_SERVICE_NAME", "")
    if not service or len(service) > 128 or any(ord(character) < 32 for character in service):
        raise ValueError("an explicit synthetic service name is required")
    receipt_path = Path(os.environ.get("RUNKITE_TEST_OTLP_RECEIPT", ""))
    if not receipt_path.is_absolute() or receipt_path.exists():
        raise ValueError("a new absolute receipt path is required")

    _isolate_http_credentials()
    sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
    from runkite_runner import tracing

    shutdown = tracing.init()
    receipt = None
    try:
        with tracing.run_span(
            "synthetic-langfuse-python-run",
            graph_id="synthetic-langfuse-agent",
            thread_id="synthetic-langfuse-python-thread",
            tenant_id="synthetic-langfuse-tenant",
            trace_context={"traceparent": parent, "correlation_id": "synthetic-langfuse-correlation"},
        ) as span:
            if span is None:
                raise RuntimeError("the real Python tracing provider was not initialized")
            callbacks = tracing.make_run_callbacks("synthetic-langfuse-python-run")
            if len(callbacks) != 1:
                raise RuntimeError("the real Python callback was unavailable")
            handler = callbacks[0]
            llm_id, tool_id = UUID(int=1), UUID(int=2)
            handler.on_chat_model_start(
                {"name": "synthetic-langfuse-model"},
                [[{"content": "synthetic-prompt-not-exported"}]],
                run_id=llm_id,
                metadata={"ls_model_name": "synthetic-langfuse-model"},
            )
            handler.on_tool_start(
                {"name": "synthetic-langfuse-tool"},
                "synthetic-tool-input-not-exported",
                run_id=tool_id,
                parent_run_id=llm_id,
            )
            handler.on_tool_end("synthetic-tool-output-not-exported", run_id=tool_id)
            handler.on_llm_end({"generations": [[{"text": "synthetic-completion-not-exported"}]]}, run_id=llm_id)
            tracing.set_run_status(span, "success")
            context = span.get_span_context()
            trace_id, span_id = f"{context.trace_id:032x}", f"{context.span_id:016x}"
            if not span.is_recording() or not context.trace_flags.sampled or trace_id != parent.split("-")[1]:
                raise RuntimeError("the synthetic run span was not sampled under its supplied parent")
            receipt = {
                "schema_version": 1,
                "trace_id": trace_id,
                "span_id": span_id,
                "traceparent": f"00-{trace_id}-{span_id}-01",
                "service": service,
                "span_name": "runkite.run",
                "protocol": "http/protobuf",
            }
    finally:
        shutdown()
    if receipt is None:
        raise RuntimeError("no local span receipt was produced")
    descriptor = os.open(receipt_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as receipt_file:
        json.dump(receipt, receipt_file, separators=(",", ":"))
        receipt_file.write("\n")


if __name__ == "__main__":
    main()
