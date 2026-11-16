/** Real OTLP/HTTP JSON export to a bounded, provider-free loopback capture. */
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { createServer, type IncomingHttpHeaders } from "node:http";
import { createRequire } from "node:module";
import { test } from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";
import { initTracing, makeRunCallbacks, setRunStatus, withRunSpan } from "./tracing.js";
import type { OTelCallbackHandler } from "./otelCallbacks.js";

const tracePath = "/api/public/otel/v1/traces";
const authorization = `Basic ${Buffer.from("pk-synthetic:sk-synthetic").toString("base64")}`;
const privateValues = [
  "synthetic-prompt-body",
  "synthetic-completion-body",
  "synthetic-tool-input",
  "synthetic-tool-output",
];
const errorValue = "synthetic-exported-exception";
const maxBody = 128 * 1024;

async function emitSpans(): Promise<void> {
  // A separate process gives initTracing real, fresh SDK globals instead of
  // installing an in-memory tracer through the test hook.
  const shutdown = await initTracing();
  try {
    await Promise.all(
      [0, 1].map(async (index) => {
        const runId = `wire-run-${index}`;
        try {
          await withRunSpan(
            {
              runId,
              graphId: "synthetic-agent",
              threadId: `wire-thread-${index}`,
              tenantId: "synthetic-tenant",
              traceContext: {
                traceparent: `00-${(index === 0 ? "11" : "33").repeat(16)}-${(index === 0 ? "22" : "44").repeat(8)}-01`,
                tracestate: `wire=case${index}`,
                correlation_id: `wire-correlation-${index}`,
              },
            },
            async (span) => {
              assert.ok(span, "real tracing initialization was disabled");
              const callbacks = makeRunCallbacks(runId);
              assert.equal(callbacks.length, 1, "real LangChain callback was unavailable");
              const handler = callbacks[0] as OTelCallbackHandler;
              const llmId = `llm-${index}`;
              const toolId = `tool-${index}`;
              const model = { name: "ignored-model", lc: 1 as const, type: "not_implemented" as const, id: ["model"] };
              if (index === 0) {
                handler.handleChatModelStart(
                  model,
                  [[{ content: privateValues[0] }]],
                  llmId,
                  undefined,
                  undefined,
                  undefined,
                  { ls_model_name: "synthetic-model" },
                );
              } else {
                handler.handleLLMStart(model, [privateValues[0]], llmId, undefined, undefined, undefined, {
                  ls_model_name: "synthetic-model",
                });
              }
              await new Promise<void>((resolve) => setImmediate(resolve));
              handler.handleToolStart(
                { name: "synthetic-tool", lc: 1, type: "not_implemented", id: ["tool"] },
                privateValues[2],
                toolId,
                llmId,
              );
              await new Promise<void>((resolve) => setImmediate(resolve));
              if (index === 1) {
                const error = new Error(errorValue);
                handler.handleToolError(error, toolId);
                handler.handleLLMError(error, llmId);
                setRunStatus(span, "error");
                throw error;
              }
              handler.handleToolEnd(privateValues[3], toolId);
              handler.handleLLMEnd({ generations: [[{ text: privateValues[1] }]] }, llmId);
              setRunStatus(span, "success");
            },
          );
        } catch (error) {
          if (index !== 1 || !(error instanceof Error) || error.message !== errorValue) throw error;
        }
      }),
    );
  } finally {
    // The scheduled batch delay is longer than the entire child deadline.
    // Capturing these spans therefore depends on shutdown draining the batch.
    await shutdown();
  }
}

function childEnvironment(port: number, signalSpecific: boolean): NodeJS.ProcessEnv {
  // Do not inherit provider credentials, OTel headers/resources, proxies,
  // NODE_OPTIONS instrumentation, or customer metadata from the host.
  const env: NodeJS.ProcessEnv = {};
  for (const key of ["PATH", "SYSTEMROOT"]) {
    if (process.env[key]) env[key] = process.env[key];
  }
  Object.assign(env, {
    NO_PROXY: "127.0.0.1,localhost",
    OTEL_SERVICE_NAME: "runkite-typescript-wire",
    OTEL_BSP_SCHEDULE_DELAY: "60000",
    OTEL_BSP_MAX_EXPORT_BATCH_SIZE: "512",
    OTEL_BSP_MAX_QUEUE_SIZE: "2048",
    OTEL_EXPORTER_OTLP_TIMEOUT: "2000",
  });
  const headers = `Authorization=${encodeURIComponent(authorization)},x-langfuse-ingestion-version=4`;
  const baseEndpoint = `http://127.0.0.1:${port}/api/public/otel`;
  if (signalSpecific) {
    Object.assign(env, {
      OTEL_EXPORTER_OTLP_ENDPOINT: `http://127.0.0.1:${port}/wrong-base`,
      OTEL_EXPORTER_OTLP_PROTOCOL: "grpc",
      OTEL_EXPORTER_OTLP_HEADERS: "Authorization=wrong,x-langfuse-ingestion-version=0",
      OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: `${baseEndpoint}/v1/traces`,
      OTEL_EXPORTER_OTLP_TRACES_PROTOCOL: "http/json",
      OTEL_EXPORTER_OTLP_TRACES_HEADERS: headers,
    });
  } else {
    Object.assign(env, {
      OTEL_EXPORTER_OTLP_ENDPOINT: baseEndpoint,
      OTEL_EXPORTER_OTLP_PROTOCOL: "http/json",
      OTEL_EXPORTER_OTLP_HEADERS: headers,
    });
  }
  return env;
}

async function runChild(env: NodeJS.ProcessEnv): Promise<void> {
  const loader = pathToFileURL(createRequire(import.meta.url).resolve("tsx")).href;
  const child = spawn(process.execPath, ["--import", loader, fileURLToPath(import.meta.url), "--emit"], {
    env,
    stdio: ["ignore", "pipe", "pipe"],
  });
  await new Promise<void>((resolve, reject) => {
    let outputBytes = 0;
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(new Error("exporter subprocess exceeded its deadline"));
    }, 20_000);
    const boundOutput = (chunk: Buffer) => {
      outputBytes += chunk.length;
      if (outputBytes > 32 * 1024) {
        child.kill("SIGKILL");
        reject(new Error("exporter subprocess exceeded its diagnostic limit"));
      }
    };
    // Drain diagnostics without writing payloads or exporter errors to CI logs.
    child.stdout.on("data", boundOutput);
    child.stderr.on("data", boundOutput);
    child.once("error", (error) => {
      clearTimeout(timer);
      reject(error);
    });
    child.once("close", (code) => {
      clearTimeout(timer);
      if (code === 0) resolve();
      else reject(new Error("exporter subprocess failed"));
    });
  });
}

type Attribute = { key: string; value: { stringValue?: string } };
type WireSpan = {
  name: string;
  traceId: string;
  spanId: string;
  parentSpanId?: string;
  traceState?: string;
  startTimeUnixNano: string;
  endTimeUnixNano: string;
  attributes: Attribute[];
  status: { code: number; message?: string };
  events: { name: string; attributes: Attribute[] }[];
};
type TraceRequest = {
  resourceSpans: {
    resource: { attributes: Attribute[] };
    scopeSpans: { scope: { name: string }; spans: WireSpan[] }[];
  }[];
};

function attributes(values: Attribute[]): Record<string, string | undefined> {
  return Object.fromEntries(values.map(({ key, value }) => [key, value.stringValue]));
}

async function assertWire(signalSpecific: boolean): Promise<void> {
  const requests: { path: string | undefined; headers: IncomingHttpHeaders; body: Buffer }[] = [];
  const errors: string[] = [];
  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    let length = 0;
    req.setTimeout(3000, () => req.destroy());
    req.on("error", () => errors.push("capture request failed"));
    req.on("data", (chunk: Buffer) => {
      length += chunk.length;
      if (length > maxBody) {
        errors.push("capture body exceeded its bound");
        req.destroy();
      } else chunks.push(chunk);
    });
    req.on("end", () => {
      if (req.method !== "POST" || requests.length >= 4 || length === 0) {
        errors.push("capture request exceeded its bounds");
        res.writeHead(400).end();
        return;
      }
      requests.push({ path: req.url, headers: req.headers, body: Buffer.concat(chunks) });
      res.writeHead(200, { "Content-Type": "application/json", Connection: "close" }).end("{}");
    });
  });
  server.requestTimeout = 4000;
  server.headersTimeout = 4000;
  try {
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    const address = server.address();
    assert.ok(address && typeof address !== "string");
    await runChild(childEnvironment(address.port, signalSpecific));
  } finally {
    server.closeAllConnections();
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
  assert.deepEqual(errors, [], "capture rejected a request");
  assert.ok(requests.length > 0, "graceful shutdown did not deliver an OTLP request");
  const spans: WireSpan[] = [];
  for (const request of requests) {
    assert.equal(request.path, tracePath);
    assert.equal(request.headers.authorization, authorization);
    assert.equal(request.headers["x-langfuse-ingestion-version"], "4");
    assert.equal(request.headers["content-type"], "application/json");
    assert.equal(request.headers["content-encoding"], undefined);
    const text = request.body.toString("utf8");
    for (const privateValue of privateValues) {
      assert.ok(!text.includes(privateValue), "normal callback body was copied to telemetry");
    }
    const body = JSON.parse(text) as TraceRequest;
    for (const resourceSpans of body.resourceSpans) {
      assert.equal(attributes(resourceSpans.resource.attributes)["service.name"], "runkite-typescript-wire");
      for (const scope of resourceSpans.scopeSpans) {
        assert.equal(scope.scope.name, "runkite.runner");
        spans.push(...scope.spans);
      }
    }
  }
  assert.equal(spans.length, 6, "missing or duplicate exported spans");
  for (const index of [0, 1]) {
    const runId = `wire-run-${index}`;
    const selected = Object.fromEntries(
      spans.filter((span) => attributes(span.attributes)["run.id"] === runId).map((span) => [span.name, span]),
    );
    assert.deepEqual(Object.keys(selected).sort(), ["runkite.llm", "runkite.run", "runkite.tool"]);
    const run = selected["runkite.run"];
    const llm = selected["runkite.llm"];
    const tool = selected["runkite.tool"];
    for (const span of Object.values(selected)) {
      assert.equal(span.traceId, (index === 0 ? "11" : "33").repeat(16));
      assert.equal(span.status.code, index === 0 ? 1 : 2);
      assert.ok(BigInt(span.endTimeUnixNano) > BigInt(span.startTimeUnixNano));
    }
    assert.equal(run.parentSpanId, (index === 0 ? "22" : "44").repeat(8));
    assert.equal(run.traceState, `wire=case${index}`);
    assert.equal(llm.parentSpanId, run.spanId);
    assert.equal(tool.parentSpanId, llm.spanId);
    const attrs = attributes(run.attributes);
    assert.equal(attrs["graph.id"], "synthetic-agent");
    assert.equal(attrs["thread.id"], `wire-thread-${index}`);
    assert.equal(attrs["tenant.id"], "synthetic-tenant");
    assert.equal(attrs["correlation.id"], `wire-correlation-${index}`);
    assert.equal(attrs["run.status"], index === 0 ? "success" : "error");
    assert.equal(attributes(llm.attributes)["llm.name"], "synthetic-model");
    assert.equal(attributes(tool.attributes)["tool.name"], "synthetic-tool");
    if (index === 1) {
      // Normal callback bodies are omitted. Raw errors are not redacted.
      for (const span of Object.values(selected)) {
        assert.equal(span.status.message, errorValue);
        assert.ok(
          span.events.some(
            (event) => event.name === "exception" && attributes(event.attributes)["exception.message"] === errorValue,
          ),
        );
      }
    }
  }
}

if (process.argv[2] === "--emit") {
  await emitSpans();
} else {
  test("OTLP JSON signal-specific endpoint, protocol and headers take precedence", { timeout: 30_000 }, async () => {
    await assertWire(true);
  });
  test("OTLP JSON general endpoint appends the trace path", { timeout: 30_000 }, async () => {
    await assertWire(false);
  });
}
