/**
 * Runner-side OpenTelemetry: nest each job under the control plane's
 * W3C traceparent from RunAssignment.trace_context.
 *
 * Disabled by default until OTEL_EXPORTER_OTLP_ENDPOINT (or
 * OTEL_EXPORTER_OTLP_TRACES_ENDPOINT) is set -- same contract as the
 * Go control plane's internal/tracing package.
 */
import { context, propagation, Span, SpanStatusCode, trace, Tracer } from "@opentelemetry/api";
import { AsyncLocalStorageContextManager } from "@opentelemetry/context-async-hooks";
import { W3CTraceContextPropagator } from "@opentelemetry/core";
import { OTLPTraceExporter as OTLPHttpExporter } from "@opentelemetry/exporter-trace-otlp-http";
import { OTLPTraceExporter as OTLPGrpcExporter } from "@opentelemetry/exporter-trace-otlp-grpc";
import { resourceFromAttributes } from "@opentelemetry/resources";
import { BasicTracerProvider, BatchSpanProcessor } from "@opentelemetry/sdk-trace-base";
import { ATTR_SERVICE_NAME } from "@opentelemetry/semantic-conventions";
import type { BaseCallbackHandler } from "@langchain/core/callbacks/base";
import { logger } from "./logger.js";
import { newOTelCallbackHandler } from "./otelCallbacks.js";

let tracer: Tracer | null = null;
let provider: BasicTracerProvider | null = null;
let contextManagerReady = false;

/** context.with / getActiveSpan need a ContextManager; without one they no-op. */
function ensureContextManager(): void {
  if (contextManagerReady) return;
  context.setGlobalContextManager(new AsyncLocalStorageContextManager());
  contextManagerReady = true;
}

/** Test-only: install (or clear) the module tracer without OTLP export. */
export function installTracerForTests(t: Tracer | null): void {
  if (t) ensureContextManager();
  tracer = t;
}

function endpointConfigured(): boolean {
  return Boolean(process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT || process.env.OTEL_EXPORTER_OTLP_ENDPOINT);
}

/** Install a TracerProvider from OTEL_* env vars. Returns a shutdown fn. */
export async function initTracing(): Promise<() => Promise<void>> {
  const noop = async () => {};
  if (!endpointConfigured()) return noop;
  if (provider) {
    return async () => {
      await provider!.shutdown();
    };
  }

  const protocol = (
    process.env.OTEL_EXPORTER_OTLP_TRACES_PROTOCOL ||
    process.env.OTEL_EXPORTER_OTLP_PROTOCOL ||
    "grpc"
  ).toLowerCase();
  const exporter = protocol.startsWith("http") ? new OTLPHttpExporter() : new OTLPGrpcExporter();

  const serviceName = process.env.OTEL_SERVICE_NAME || "runkite-runner";
  ensureContextManager();
  provider = new BasicTracerProvider({
    resource: resourceFromAttributes({ [ATTR_SERVICE_NAME]: serviceName }),
    spanProcessors: [new BatchSpanProcessor(exporter)],
  });
  trace.setGlobalTracerProvider(provider);
  propagation.setGlobalPropagator(new W3CTraceContextPropagator());
  tracer = trace.getTracer("runkite.runner");
  logger.info(`OpenTelemetry tracing enabled (protocol=${protocol} service=${serviceName})`);

  return async () => {
    await provider!.shutdown();
  };
}

export type TraceContextFields = {
  correlation_id?: string;
  traceparent?: string;
  tracestate?: string;
};

/** Run fn under a child span of the CP-provided traceparent (when enabled). */
export async function withRunSpan<T>(
  opts: {
    runId: string;
    graphId?: string;
    threadId?: string;
    tenantId?: string;
    traceContext?: TraceContextFields;
  },
  fn: (span: Span | null) => Promise<T>,
): Promise<T> {
  if (!tracer) {
    return fn(null);
  }

  const carrier: Record<string, string> = {};
  if (opts.traceContext?.traceparent) carrier.traceparent = opts.traceContext.traceparent;
  if (opts.traceContext?.tracestate) carrier.tracestate = opts.traceContext.tracestate;
  const parentCtx = Object.keys(carrier).length > 0 ? propagation.extract(context.active(), carrier) : context.active();

  const span = tracer.startSpan(
    "runkite.run",
    {
      attributes: {
        "run.id": opts.runId,
        ...(opts.graphId ? { "graph.id": opts.graphId } : {}),
        ...(opts.threadId ? { "thread.id": opts.threadId } : {}),
        ...(opts.tenantId ? { "tenant.id": opts.tenantId } : {}),
        ...(opts.traceContext?.correlation_id ? { "correlation.id": opts.traceContext.correlation_id } : {}),
      },
    },
    parentCtx,
  );

  try {
    // Activate on parentCtx (carries remote parent) so getActiveSpan() and
    // makeRunCallbacks see this job span under --concurrency > 1 (ALS).
    return await context.with(trace.setSpan(parentCtx, span), () => fn(span));
  } catch (err) {
    span.setStatus({ code: SpanStatusCode.ERROR, message: err instanceof Error ? err.message : String(err) });
    span.recordException(err instanceof Error ? err : new Error(String(err)));
    throw err;
  } finally {
    span.end();
  }
}

export function setRunStatus(span: Span | null, status: string): void {
  if (!span) return;
  span.setAttribute("run.status", status);
  span.setStatus({
    code: status === "error" ? SpanStatusCode.ERROR : SpanStatusCode.OK,
  });
}

/**
 * LangChain handlers that open LLM/tool child spans under runkite.run.
 * Empty when tracing is disabled. Call inside an active withRunSpan so
 * the captured parent context is the job span.
 */
export function makeRunCallbacks(runId: string = ""): BaseCallbackHandler[] {
  if (!tracer) return [];
  return [newOTelCallbackHandler(tracer, runId)];
}
