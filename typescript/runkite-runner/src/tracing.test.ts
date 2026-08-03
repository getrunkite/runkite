/**
 * Self-check for tracing.ts: no-op without a tracer; child span under
 * a parent traceparent when an in-memory provider is installed.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { propagation, trace } from "@opentelemetry/api";
import { W3CTraceContextPropagator } from "@opentelemetry/core";
import { InMemorySpanExporter, SimpleSpanProcessor, BasicTracerProvider } from "@opentelemetry/sdk-trace-base";
import { installTracerForTests, setRunStatus, withRunSpan } from "./tracing.js";

test("withRunSpan is a no-op when tracing was never initialized", async () => {
  installTracerForTests(null);
  let saw: unknown = "unset";
  await withRunSpan({ runId: "r1", graphId: "g" }, async (span) => {
    saw = span;
  });
  assert.equal(saw, null);
});

test("withRunSpan nests under parent traceparent", async () => {
  const exporter = new InMemorySpanExporter();
  const provider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(exporter)],
  });
  trace.setGlobalTracerProvider(provider);
  propagation.setGlobalPropagator(new W3CTraceContextPropagator());
  installTracerForTests(trace.getTracer("runkite.runner"));

  const parentTp = "00-" + "11".repeat(16) + "-" + "22".repeat(8) + "-01";
  await withRunSpan(
    {
      runId: "run-otel",
      graphId: "echo",
      threadId: "t1",
      tenantId: "acme",
      traceContext: { traceparent: parentTp, correlation_id: "corr-1" },
    },
    async (span) => {
      assert.ok(span);
      setRunStatus(span, "success");
    },
  );

  const finished = exporter.getFinishedSpans();
  assert.equal(finished.length, 1);
  assert.equal(finished[0].name, "runkite.run");
  assert.equal(finished[0].attributes["run.id"], "run-otel");
  assert.equal(finished[0].spanContext().traceId, "11".repeat(16));
  assert.equal(finished[0].parentSpanContext?.spanId, "22".repeat(8));
  assert.equal(finished[0].attributes["run.status"], "success");

  installTracerForTests(null);
  await provider.shutdown();
});
