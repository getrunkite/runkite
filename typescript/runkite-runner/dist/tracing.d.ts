/**
 * Runner-side OpenTelemetry: nest each job under the control plane's
 * W3C traceparent from RunAssignment.trace_context.
 *
 * Disabled by default until OTEL_EXPORTER_OTLP_ENDPOINT (or
 * OTEL_EXPORTER_OTLP_TRACES_ENDPOINT) is set -- same contract as the
 * Go control plane's internal/tracing package.
 */
import { Span, Tracer } from "@opentelemetry/api";
/** Test-only: install (or clear) the module tracer without OTLP export. */
export declare function installTracerForTests(t: Tracer | null): void;
/** Install a TracerProvider from OTEL_* env vars. Returns a shutdown fn. */
export declare function initTracing(): Promise<() => Promise<void>>;
export type TraceContextFields = {
    correlation_id?: string;
    traceparent?: string;
    tracestate?: string;
};
/** Run fn under a child span of the CP-provided traceparent (when enabled). */
export declare function withRunSpan<T>(opts: {
    runId: string;
    graphId?: string;
    threadId?: string;
    tenantId?: string;
    traceContext?: TraceContextFields;
}, fn: (span: Span | null) => Promise<T>): Promise<T>;
export declare function setRunStatus(span: Span | null, status: string): void;
