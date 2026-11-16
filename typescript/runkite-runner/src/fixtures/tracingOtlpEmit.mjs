/** Three synthetic spans through the built runner, never a model provider. */
import { open } from "node:fs/promises";
import { isAbsolute } from "node:path";

if (process.env.RUNKITE_TEST_OTLP_EMIT !== "1") {
  throw new Error("explicit synthetic emitter mode is required");
}
const endpoint = new URL(process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT || "invalid:");
if (
  endpoint.protocol !== "http:" ||
  endpoint.hostname !== "127.0.0.1" ||
  !endpoint.port ||
  Number(endpoint.port) <= 0 ||
  endpoint.username ||
  endpoint.password ||
  endpoint.search ||
  endpoint.hash ||
  endpoint.pathname !== "/api/public/otel/v1/traces"
) {
  throw new Error("a full owned loopback OTLP trace endpoint is required");
}
const protocol = process.env.OTEL_EXPORTER_OTLP_TRACES_PROTOCOL || process.env.OTEL_EXPORTER_OTLP_PROTOCOL;
if (protocol !== "http/json") throw new Error("the TypeScript emitter requires explicit http/json");
const parent = process.env.RUNKITE_TEST_OTLP_PARENT || "";
if (!/^00-(?!0{32})[0-9a-f]{32}-(?!0{16})[0-9a-f]{16}-01$/.test(parent)) {
  throw new Error("a sampled, nonzero W3C parent is required");
}
const service = process.env.OTEL_SERVICE_NAME || "";
if (!service || service.length > 128 || Array.from(service).some((character) => character.charCodeAt(0) < 32)) {
  throw new Error("an explicit synthetic service name is required");
}
const receiptPath = process.env.RUNKITE_TEST_OTLP_RECEIPT || "";
if (!isAbsolute(receiptPath)) throw new Error("a new absolute receipt path is required");

// Import actual build output. No exporter or tracer implementation is mocked.
const { initTracing, makeRunCallbacks, setRunStatus, withRunSpan } = await import("../../dist/tracing.js");
const shutdown = await initTracing();
let receipt;
try {
  await withRunSpan(
    {
      runId: "synthetic-langfuse-typescript-run",
      graphId: "synthetic-langfuse-agent",
      threadId: "synthetic-langfuse-typescript-thread",
      tenantId: "synthetic-langfuse-tenant",
      traceContext: { traceparent: parent, correlation_id: "synthetic-langfuse-correlation" },
    },
    async (span) => {
      if (!span) throw new Error("the real TypeScript tracing provider was not initialized");
      const callbacks = makeRunCallbacks("synthetic-langfuse-typescript-run");
      if (callbacks.length !== 1) throw new Error("the real TypeScript callback was unavailable");
      const handler = callbacks[0];
      handler.handleChatModelStart(
        { name: "synthetic-langfuse-model", lc: 1, type: "not_implemented", id: ["synthetic-langfuse-model"] },
        [[{ content: "synthetic-prompt-not-exported" }]],
        "synthetic-llm",
        undefined,
        undefined,
        undefined,
        { ls_model_name: "synthetic-langfuse-model" },
      );
      handler.handleToolStart(
        { name: "synthetic-langfuse-tool", lc: 1, type: "not_implemented", id: ["synthetic-langfuse-tool"] },
        "synthetic-tool-input-not-exported",
        "synthetic-tool",
        "synthetic-llm",
      );
      handler.handleToolEnd("synthetic-tool-output-not-exported", "synthetic-tool");
      handler.handleLLMEnd({ generations: [[{ text: "synthetic-completion-not-exported" }]] }, "synthetic-llm");
      setRunStatus(span, "success");
      const { traceId, spanId, traceFlags } = span.spanContext();
      if (!span.isRecording() || !(traceFlags & 1) || traceId !== parent.split("-")[1]) {
        throw new Error("the synthetic run span was not sampled under its supplied parent");
      }
      receipt = {
        schema_version: 1,
        trace_id: traceId,
        span_id: spanId,
        traceparent: `00-${traceId}-${spanId}-01`,
        service,
        span_name: "runkite.run",
        protocol: "http/json",
      };
    },
  );
} finally {
  await shutdown();
}
if (!receipt) throw new Error("no local span receipt was produced");
// Exclusive creation rejects a stale receipt and symlinks. Do not copy auth
// headers or telemetry bodies into receipts or console output.
const receiptFile = await open(receiptPath, "wx", 0o600);
try {
  await receiptFile.writeFile(`${JSON.stringify(receipt)}\n`);
} finally {
  await receiptFile.close();
}
