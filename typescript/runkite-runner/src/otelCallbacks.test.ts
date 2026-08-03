/**
 * Self-check: LangChain OTel callbacks nest LLM/tool spans under runkite.run.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { InMemorySpanExporter, SimpleSpanProcessor, BasicTracerProvider } from "@opentelemetry/sdk-trace-base";
import { buildRunConfig } from "./executeRun.js";
import { installTracerForTests, makeRunCallbacks, setRunStatus, withRunSpan } from "./tracing.js";
import { OTelCallbackHandler } from "./otelCallbacks.js";

test("makeRunCallbacks is empty when tracing is disabled", () => {
  installTracerForTests(null);
  assert.deepEqual(makeRunCallbacks("r1"), []);
});

test("buildRunConfig merges otel callbacks with existing ones", async () => {
  const exporter = new InMemorySpanExporter();
  const provider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(exporter)],
  });
  // Bind the module tracer to this provider directly -- avoid
  // setGlobalTracerProvider (first registration wins across the suite).
  installTracerForTests(provider.getTracer("runkite.runner"));

  const existing = { name: "existing-handler" };
  const config = buildRunConfig({
    run_id: "r-merge",
    thread_id: "t1",
    graph_id: "g",
    config: { callbacks: [existing] },
  });
  const cbs = config.callbacks as unknown[];
  assert.equal(cbs.length, 2);
  assert.equal(cbs[0], existing);
  assert.ok(cbs[1] instanceof OTelCallbackHandler);

  installTracerForTests(null);
  await provider.shutdown();
});

test("llm and tool spans nest under runkite.run", async () => {
  const exporter = new InMemorySpanExporter();
  const provider = new BasicTracerProvider({
    spanProcessors: [new SimpleSpanProcessor(exporter)],
  });
  installTracerForTests(provider.getTracer("runkite.runner"));

  const llmId = "llm-run-1";
  const toolId = "tool-run-1";

  await withRunSpan({ runId: "run-cb", graphId: "g" }, async (span) => {
    assert.ok(span);
    const cbs = makeRunCallbacks("run-cb");
    assert.equal(cbs.length, 1);
    const h = cbs[0] as OTelCallbackHandler;
    h.handleChatModelStart(
      { name: "fake-chat", lc: 1, type: "not_implemented", id: ["fake-chat"] },
      [[]],
      llmId,
      undefined,
      undefined,
      undefined,
      { ls_model_name: "gpt-test" },
    );
    h.handleToolStart({ name: "search", lc: 1, type: "not_implemented", id: ["search"] }, "{}", toolId, llmId);
    h.handleToolEnd("ok", toolId);
    h.handleLLMEnd({ generations: [] }, llmId);
    setRunStatus(span, "success");
  });

  const finished = exporter.getFinishedSpans();
  const byName = Object.fromEntries(finished.map((s) => [s.name, s]));
  assert.ok(byName["runkite.run"], `spans=${finished.map((s) => s.name).join(",")}`);
  assert.ok(byName["runkite.llm"]);
  assert.ok(byName["runkite.tool"]);

  const runS = byName["runkite.run"];
  const llmS = byName["runkite.llm"];
  const toolS = byName["runkite.tool"];
  assert.equal(llmS.parentSpanContext?.spanId, runS.spanContext().spanId);
  assert.equal(toolS.parentSpanContext?.spanId, llmS.spanContext().spanId);
  assert.equal(llmS.spanContext().traceId, runS.spanContext().traceId);
  assert.equal(llmS.attributes["llm.name"], "gpt-test");
  assert.equal(toolS.attributes["tool.name"], "search");
  assert.equal(llmS.attributes["run.id"], "run-cb");

  installTracerForTests(null);
  await provider.shutdown();
});
