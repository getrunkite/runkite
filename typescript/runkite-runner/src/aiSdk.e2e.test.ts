/** Real Go plane and packaged Node runner. Offline model, SQLite, in-process queue. */
import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createServer as createHttpServer } from "node:http";
import { createServer } from "node:net";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const binary = process.env.RUNKITE_TEST_BINARY;
const testPostgresDsn = process.env.RUNKITE_TEST_POSTGRES_DSN;
const testRedisUrl = process.env.RUNKITE_TEST_REDIS_URL;
const profile = testPostgresDsn && testRedisUrl ? "Postgres + Redis" : "SQLite + in-process";
const sourceDirectory = path.dirname(fileURLToPath(import.meta.url));
const runnerDirectory = path.dirname(sourceDirectory);

type ProcessHandle = { child: ChildProcess; log: () => string; stop: () => Promise<void> };

function launch(command: string, args: string[], cwd: string, env: NodeJS.ProcessEnv): ProcessHandle {
  const child = spawn(command, args, { cwd, env, stdio: ["ignore", "pipe", "pipe"] });
  let output = "";
  const append = (chunk: Buffer) => {
    output = (output + chunk.toString()).slice(-64_000);
  };
  child.stdout?.on("data", append);
  child.stderr?.on("data", append);
  child.on("error", (error) => {
    output += `\nspawn failed: ${error.message}`;
  });
  return {
    child,
    log: () => output,
    async stop() {
      if (!child.pid || child.exitCode !== null || child.signalCode !== null) return;
      const exited = once(child, "exit").catch(() => {});
      child.kill("SIGTERM");
      await Promise.race([exited, delay(1500)]);
      if (child.exitCode === null && child.signalCode === null) {
        child.kill("SIGKILL");
        await exited;
      }
    },
  };
}

async function freePort(): Promise<number> {
  const server = createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  await new Promise<void>((resolve, reject) => server.close((error) => (error ? reject(error) : resolve())));
  return address.port;
}

async function waitFor<T>(description: string, operation: () => Promise<T | false>, timeout = 15_000): Promise<T> {
  const deadline = Date.now() + timeout;
  let lastError: unknown;
  while (Date.now() < deadline) {
    try {
      const value = await operation();
      if (value !== false) return value;
    } catch (error) {
      lastError = error;
    }
    await delay(40);
  }
  throw new Error(`Timed out waiting for ${description}`, { cause: lastError });
}

const modelFactorySource = `
import { appendFileSync, existsSync } from 'node:fs';
import { tool, jsonSchema } from 'ai';
import { MockLanguageModelV4 } from 'ai/test';

const usage = {
  inputTokens: { total: 10, noCache: 10, cacheRead: 0, cacheWrite: 0 },
  outputTokens: { total: 2, text: 2, reasoning: 0 },
};
function stream(parts, stall = false) {
  return { stream: new ReadableStream({ start(controller) {
    for (const part of parts) controller.enqueue(part);
    if (!stall) controller.close();
  } }) };
}
function textParts(text) {
  return [
    { type: 'stream-start', warnings: [] },
    { type: 'text-start', id: 'text' },
    { type: 'text-delta', id: 'text', delta: text },
    { type: 'text-end', id: 'text' },
    { type: 'finish', finishReason: { unified: 'stop', raw: 'stop' }, usage },
  ];
}
function heldStream(parts, signal) {
  let cancelled = false;
  return { stream: new ReadableStream({
    async start(controller) {
      for (const part of parts.slice(0, 3)) controller.enqueue(part);
      while (!existsSync(process.env.E2E_RELEASE_FILE) && !cancelled && !signal.aborted) {
        await new Promise(resolve => setTimeout(resolve, 20));
      }
      if (cancelled) return;
      if (!signal.aborted) for (const part of parts.slice(3)) controller.enqueue(part);
      controller.close();
    },
    cancel() { cancelled = true; },
  }) };
}
function buildAgent(context: { runId: string; signal: AbortSignal }, hold = false) {
  let step = 0;
  return {
    model: new MockLanguageModelV4({ modelId: 'e2e-offline-model', doStream: async (options) => {
      const roles = options.prompt.map(message => message.role);
      appendFileSync(process.env.E2E_CALLS_FILE, JSON.stringify({ runId: context.runId, roles }) + '\\n');
      if (step++ === 0) return stream([
        { type: 'stream-start', warnings: [] },
        { type: 'tool-input-start', id: 'note-call', toolName: 'createNote' },
        { type: 'tool-input-delta', id: 'note-call', delta: '{"title":"The original note"}' },
        { type: 'tool-input-end', id: 'note-call' },
        { type: 'tool-call', toolCallId: 'note-call', toolName: 'createNote', input: '{"title":"The original note"}' },
        { type: 'finish', finishReason: { unified: 'tool-calls', raw: 'tool-calls' }, usage },
      ]);
      const users = roles.filter(role => role === 'user').length;
      const parts = textParts(users > 1 ? 'The answer (history has ' + users + ' user messages)' : 'The answer');
      return hold ? heldStream(parts, context.signal) : stream(parts);
    } }),
    maxRetries: 0,
    tools: { createNote: tool({
      inputSchema: jsonSchema({ type: 'object', properties: { title: { type: 'string' } }, required: ['title'] }),
      execute: async ({ title }) => {
        appendFileSync(process.env.E2E_EFFECTS_FILE, JSON.stringify({ runId: context.runId, title }) + '\\n');
        return { noteId: 'note-one' };
      },
    }) },
  };
}
export function agent(context: { runId: string; signal: AbortSignal }) {
  return buildAgent(context);
}
export function reconnect(context: { runId: string; signal: AbortSignal }) {
  context.signal.addEventListener('abort', () => {
    appendFileSync(process.env.E2E_CANCEL_FILE, context.runId + '\\n');
  }, { once: true });
  return buildAgent(context, true);
}
export function slow(context: { runId: string; signal: AbortSignal }) {
  context.signal.addEventListener('abort', () => {
    appendFileSync(process.env.E2E_CANCEL_FILE, context.runId + '\\n');
  }, { once: true });
  return {
    model: new MockLanguageModelV4({ modelId: 'e2e-offline-model', doStream: async () => {
      appendFileSync(process.env.E2E_CALLS_FILE, JSON.stringify({ runId: context.runId, slow: true }) + '\\n');
      return stream([
        { type: 'stream-start', warnings: [] },
        { type: 'text-start', id: 'slow' },
        { type: 'text-delta', id: 'slow', delta: 'Waiting for cancellation' },
      ], true);
    } }),
    maxRetries: 0,
  };
}
`;

test(
  `managed AI SDK runs through a real control plane and packaged runner (${profile})`,
  {
    skip: binary ? false : "Set RUNKITE_TEST_BINARY to a built runkite binary for subprocess integration coverage",
    timeout: 180_000,
  },
  async (t) => {
    assert.equal(
      Boolean(testPostgresDsn),
      Boolean(testRedisUrl),
      "Set RUNKITE_TEST_POSTGRES_DSN and RUNKITE_TEST_REDIS_URL together for the dedicated external test profile",
    );
    const directory = await mkdtemp(path.join(runnerDirectory, ".ai-sdk-e2e-"));
    let plane: ProcessHandle | undefined;
    let runner: ProcessHandle | undefined;
    t.after(async () => {
      await runner?.stop();
      await plane?.stop();
      await rm(directory, { recursive: true, force: true });
    });
    const callsFile = path.join(directory, "calls.jsonl");
    const effectsFile = path.join(directory, "effects.jsonl");
    const cancelFile = path.join(directory, "cancelled.txt");
    const releaseFile = path.join(directory, "release-model");
    await writeFile(path.join(directory, "agent.ts"), modelFactorySource);
    const config = path.join(directory, "runkite.json");
    await writeFile(
      config,
      JSON.stringify({
        runner_kind: "typescript-ai-sdk",
        graphs: {
          e2e_agent: "./agent.ts:agent",
          e2e_slow: "./agent.ts:slow",
          e2e_denied: "./agent.ts:agent",
          e2e_reconnect: "./agent.ts:reconnect",
        },
        allowed_tools: { e2e_denied: [] },
      }),
    );
    const [httpPort, grpcPort] = await Promise.all([freePort(), freePort()]);
    const baseUrl = `http://127.0.0.1:${httpPort}`;
    // Deliberately omit inherited DB, provider, auth and telemetry configuration.
    const environment: NodeJS.ProcessEnv = {
      PATH: process.env.PATH,
      LANG: process.env.LANG ?? "C.UTF-8",
      TMPDIR: process.env.TMPDIR,
      DATABASE_PATH: path.join(directory, "state.db"),
      LOG_FORMAT: "json",
      RUNKITE_ALLOW_INSECURE_SERVE: "1",
      RUNNER_TOKEN_TYPESCRIPT_AI_SDK: "e2e-runner-token",
      RUNNER_TENANTS_TYPESCRIPT_AI_SDK: "default",
      RUNKITE_HTTP_URL: baseUrl,
      E2E_CALLS_FILE: callsFile,
      E2E_EFFECTS_FILE: effectsFile,
      E2E_CANCEL_FILE: cancelFile,
      E2E_RELEASE_FILE: releaseFile,
    };
    plane = launch(
      path.resolve(binary!),
      ["dev", "--config", config, "--port", String(httpPort), "--grpc-port", String(grpcPort)],
      directory,
      {
        ...environment,
        ...(testPostgresDsn && testRedisUrl ? { POSTGRES_DSN: testPostgresDsn, REDIS_URL: testRedisUrl } : {}),
      },
    );
    const previousRunnerLogs: string[] = [];
    const logs = () =>
      `Control plane:\n${plane?.log()}\nRunner:\n${[...previousRunnerLogs, runner?.log() ?? ""].join("\n")}`;
    let failed = false;
    const runCase = async (name: string, operation: () => Promise<void>) => {
      if (failed) {
        await t.test(name, { skip: "An earlier subprocess assertion failed" });
        return;
      }
      await t.test(name, async (subtest) => {
        let timer: ReturnType<typeof setTimeout> | undefined;
        try {
          await Promise.race([
            operation(),
            new Promise<never>((_resolve, reject) => {
              timer = setTimeout(() => reject(new Error(`Subprocess scenario timed out: ${name}`)), 25_000);
            }),
          ]);
        } catch (error) {
          failed = true;
          subtest.diagnostic(logs());
          throw error;
        } finally {
          if (timer) clearTimeout(timer);
        }
      });
    };
    const request = async (route: string, body?: unknown) => {
      const response = await fetch(baseUrl + route, {
        method: body === undefined ? "GET" : "POST",
        headers: body === undefined ? undefined : { "Content-Type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(15_000),
      });
      const text = await response.text();
      assert.ok(response.ok, `${route}: HTTP ${response.status} ${text}`);
      return text ? JSON.parse(text) : undefined;
    };
    const startRunner = async (httpAddress = baseUrl) => {
      runner = launch(
        process.execPath,
        [
          path.join(runnerDirectory, "dist", "cli.js"),
          "--framework",
          "ai-sdk",
          "--config",
          config,
          "--grpc-address",
          `127.0.0.1:${grpcPort}`,
          "--http-address",
          httpAddress,
        ],
        directory,
        { ...environment, RUNKITE_HTTP_URL: httpAddress, RUNNER_TOKEN: "e2e-runner-token" },
      );
      await waitFor("runner startup", async () => {
        assert.equal(runner!.child.exitCode, null, `runner exited: ${runner!.log()}`);
        return runner!.log().includes("Worker ready") || false;
      });
    };
    const createThread = async () => (await request("/threads", {})).thread_id as string;
    const createRun = async (threadId: string, agentId: string, content: string) =>
      request(`/threads/${threadId}/runs`, {
        agent_id: agentId,
        input: { messages: [{ role: "user", content }] },
        stream_mode: ["messages", "values", "custom"],
      });
    const completedRun = async (threadId: string, runId: string) =>
      waitFor("durable run completion", async () => {
        const run = await request(`/threads/${threadId}/runs/${runId}`);
        return ["success", "error", "interrupted"].includes(run.status) ? run : false;
      });
    const readJsonLines = async (file: string) => {
      try {
        return (await readFile(file, "utf8"))
          .trim()
          .split("\n")
          .filter(Boolean)
          .map((line) => JSON.parse(line));
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENOENT") return [];
        throw error;
      }
    };
    try {
      await waitFor("control plane readiness", async () => {
        const response = await fetch(`${baseUrl}/readyz`, { signal: AbortSignal.timeout(500) });
        return response.ok || false;
      });
      await startRunner();
      const threadId = await createThread();
      let firstRunId = "";

      await runCase("dispatch, persisted output, Admin visibility, custom UI events and usage", async () => {
        const created = await createRun(threadId, "e2e_agent", "Create the first note");
        firstRunId = created.run_id;
        const final = await completedRun(threadId, firstRunId);
        assert.equal(final.status, "success", JSON.stringify(final));
        assert.equal(final.output.usage.prompt_tokens, 20);
        assert.equal(final.output.usage.completion_tokens, 4);
        assert.equal(final.output.messages.at(-1).content[0].text, "The answer");
        const thread = await request(`/threads/${threadId}`);
        assert.deepEqual(thread.values, final.output);
        const runs = await request("/admin-api/runs");
        assert.ok(runs.some((run: { run_id: string }) => run.run_id === firstRunId));
        const threads = await request("/admin-api/threads");
        assert.ok(threads.some((row: { thread_id: string }) => row.thread_id === threadId));
        const adminRun = await request(`/admin-api/runs/${firstRunId}`);
        assert.equal(adminRun.status, "success");
        const stream = await fetch(`${baseUrl}/threads/${threadId}/runs/${firstRunId}/stream`, {
          signal: AbortSignal.timeout(5000),
        });
        assert.equal(stream.ok, true);
        const events = await stream.text();
        assert.match(events, /custom:ai_sdk/);
        assert.match(events, /tool-output-available/);
        assert.match(events, /The answer/);
        assert.match(events, /"type":"finish"/);
      });

      await runCase(
        "a fresh runner restores native tool history for the next turn without billing old usage",
        async () => {
          await runner!.stop();
          previousRunnerLogs.push(runner!.log());
          await startRunner();
          const created = await createRun(threadId, "e2e_agent", "Remember the first note and create another");
          const final = await completedRun(threadId, created.run_id);
          assert.equal(final.status, "success", JSON.stringify(final));
          assert.equal(final.output.messages.at(-1).content[0].text, "The answer (history has 2 user messages)");
          const calls = await readJsonLines(callsFile);
          const restored = calls.find((call) => call.runId === created.run_id);
          assert.deepEqual(restored.roles, ["user", "assistant", "tool", "assistant", "user"]);
          assert.equal(final.output.usage.total_tokens, 24);
          // Run status commits before retryable usage finalization. Observe
          // settlement instead of racing that independent durable write.
          await waitFor("both turns' settled usage", async () => {
            const summary = await request("/admin-api/usage/summary?agent_id=e2e_agent");
            assert.equal(summary.length, 1);
            assert.equal(summary[0].tokens_in, 40);
            assert.equal(summary[0].tokens_out, 8);
            assert.equal(summary[0].run_count, 2);
            return true;
          });
          const effects = await readJsonLines(effectsFile);
          assert.equal(effects.length, 2);
        },
      );

      await runCase("cancel propagates through WatchCancels into the active model", async () => {
        const slowThread = await createThread();
        const created = await createRun(slowThread, "e2e_slow", "Wait until cancelled");
        await waitFor("slow agent execution", async () => {
          const calls = await readJsonLines(callsFile);
          return calls.some((call) => call.runId === created.run_id && call.slow === true) || false;
        });
        await request(`/threads/${slowThread}/runs/${created.run_id}/cancel`, {});
        await waitFor("runner abort signal", async () => {
          try {
            return (await readFile(cancelFile, "utf8")).includes(created.run_id) || false;
          } catch {
            return false;
          }
        });
        assert.match(runner!.log(), new RegExp(`Cancel signal received via gRPC for run ${created.run_id}`));
        const final = await completedRun(slowThread, created.run_id);
        assert.equal(final.status, "interrupted");
        await waitFor(
          "thread released after cancel",
          async () => (await request(`/threads/${slowThread}`)).status !== "busy" || false,
        );
      });

      await runCase("the plane's allowed_tools assignment blocks native tool side effects", async () => {
        const effectsBefore = (await readJsonLines(effectsFile)).length;
        const deniedThread = await createThread();
        const created = await createRun(deniedThread, "e2e_denied", "Try the blocked tool");
        const final = await completedRun(deniedThread, created.run_id);
        assert.equal(final.status, "error", JSON.stringify(final));
        assert.equal((await readJsonLines(effectsFile)).length, effectsBefore);
      });

      await runCase("native AI SDK chat transport consumes the real managed run", async () => {
        const helper = await import(
          new URL("../../../examples/managed_ai_sdk/bridge.e2e-helper.ts", import.meta.url).href
        );
        await helper.verifyManagedBridge({ baseUrl, threadId: await createThread(), agentId: "e2e_agent" });
      });

      await runCase(
        "detached managed chat attaches to its running and completed run without another execution",
        async () => {
          const helper = await import(
            new URL("../../../examples/managed_ai_sdk/bridge.e2e-helper.ts", import.meta.url).href
          );
          const reconnectThread = await createThread();
          const threadIds = (rows: Array<{ thread_id: string }>) => rows.map((row) => row.thread_id).sort();
          const beforeThreads = threadIds(await request("/admin-api/threads"));
          const usageTotals = (rows: Array<{ run_count: number; tokens_in: number; tokens_out: number }>) =>
            rows.reduce(
              (total, row) => ({
                runs: total.runs + row.run_count,
                input: total.input + row.tokens_in,
                output: total.output + row.tokens_out,
              }),
              { runs: 0, input: 0, output: 0 },
            );
          const beforeUsage = usageTotals(await request("/admin-api/usage/summary?agent_id=e2e_reconnect"));
          const runId = await helper.verifyManagedReconnectBridge({
            baseUrl,
            threadId: reconnectThread,
            agentId: "e2e_reconnect",
            assertDetached: async (activeRunId: string) => {
              const run = await request(`/threads/${reconnectThread}/runs/${activeRunId}`);
              assert.ok(["pending", "running"].includes(run.status), "detaching must not settle the running model");
              const calls = (await readJsonLines(callsFile)).filter((call) => call.runId === activeRunId);
              const effects = (await readJsonLines(effectsFile)).filter((effect) => effect.runId === activeRunId);
              assert.equal(calls.length, 2, "the one agent execution has reached its second model step");
              assert.equal(effects.length, 1, "the native tool executes once before disconnect");
              try {
                assert.equal((await readFile(cancelFile, "utf8")).includes(activeRunId), false);
              } catch (error) {
                if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
              }
            },
            release: async () => {
              await writeFile(releaseFile, "complete this one model execution");
            },
          });
          const final = await completedRun(reconnectThread, runId);
          assert.equal(final.status, "success");
          const runs = await request(`/threads/${reconnectThread}/runs`);
          assert.equal(runs.length, 1, "reattaching must not create another managed run");
          assert.equal(runs[0].run_id, runId);
          assert.deepEqual(threadIds(await request("/admin-api/threads")), beforeThreads);
          assert.equal((await readJsonLines(callsFile)).filter((call) => call.runId === runId).length, 2);
          assert.equal((await readJsonLines(effectsFile)).filter((effect) => effect.runId === runId).length, 1);
          await waitFor("one usage settlement after every attach", async () => {
            assert.deepEqual(usageTotals(await request("/admin-api/usage/summary?agent_id=e2e_reconnect")), {
              runs: beforeUsage.runs + 1,
              input: beforeUsage.input + 20,
              output: beforeUsage.output + 4,
            });
            return true;
          });
        },
      );

      await runCase("an explicitly stopped detached chat cancels its existing managed run", async () => {
        const helper = await import(
          new URL("../../../examples/managed_ai_sdk/bridge.e2e-helper.ts", import.meta.url).href
        );
        const detachedThread = await createThread();
        const runId = await helper.verifyManagedExplicitCancelBridge({
          baseUrl,
          threadId: detachedThread,
          agentId: "e2e_slow",
        });
        await waitFor("explicit stop reaches the detached model", async () => {
          try {
            return (await readFile(cancelFile, "utf8")).includes(runId) || false;
          } catch {
            return false;
          }
        });
        const final = await completedRun(detachedThread, runId);
        assert.equal(final.status, "interrupted");
        const runs = await request(`/threads/${detachedThread}/runs`);
        assert.equal(runs.length, 1);
        assert.equal(runs[0].run_id, runId);
        assert.equal((await readJsonLines(callsFile)).filter((call) => call.runId === runId).length, 1);
        assert.equal((await readJsonLines(effectsFile)).filter((effect) => effect.runId === runId).length, 0);
      });

      await runCase(
        "crash after native state commit reclaims the receipt without repeating model or tool work",
        async () => {
          const recoveryThread = await createThread();
          const usageTotals = (rows: Array<{ run_count: number; tokens_in: number; tokens_out: number }>) =>
            rows.reduce(
              (total, row) => ({
                runs: total.runs + row.run_count,
                input: total.input + row.tokens_in,
                output: total.output + row.tokens_out,
              }),
              { runs: 0, input: 0, output: 0 },
            );
          const beforeUsage = usageTotals(await request("/admin-api/usage/summary?agent_id=e2e_agent"));
          let committed: { runId: string; receipt: { completed: { run_id: string } } } | undefined;
          let relayError: unknown;
          const statePath = `/internal/checkpoints/${encodeURIComponent(recoveryThread)}/adapter-state`;
          const relay = createHttpServer((incoming, outgoing) => {
            void (async () => {
              const parts: Buffer[] = [];
              for await (const part of incoming) parts.push(Buffer.from(part));
              const body = Buffer.concat(parts);
              const headers = new Headers();
              for (const [name, value] of Object.entries(incoming.headers)) {
                if (
                  value !== undefined &&
                  !["host", "connection", "content-length", "transfer-encoding"].includes(name)
                ) {
                  headers.set(name, Array.isArray(value) ? value.join(", ") : value);
                }
              }
              const upstream = await fetch(baseUrl + incoming.url, {
                method: incoming.method,
                headers,
                body: body.length ? body : undefined,
                signal: AbortSignal.timeout(10_000),
              });
              const responseBody = Buffer.from(await upstream.arrayBuffer());
              if (incoming.method === "PUT" && incoming.url === statePath && upstream.ok && !committed) {
                // Go has committed the receipt, but the runner has not received
                // any successful response and therefore cannot publish success.
                const crashed = runner!;
                const exited = once(crashed.child, "exit");
                crashed.child.kill("SIGKILL");
                await exited;
                previousRunnerLogs.push(crashed.log());
                outgoing.destroy();
                committed = {
                  runId: headers.get("X-Runkite-Run-Id") ?? "",
                  receipt: JSON.parse(body.toString("utf8")),
                };
                return;
              }
              outgoing.writeHead(upstream.status, Object.fromEntries(upstream.headers));
              outgoing.end(responseBody);
            })().catch((error) => {
              relayError = error;
              outgoing.destroy();
            });
          });
          relay.listen(0, "127.0.0.1");
          await once(relay, "listening");
          const relayAddress = relay.address();
          assert.ok(relayAddress && typeof relayAddress !== "string");
          try {
            await runner!.stop();
            previousRunnerLogs.push(runner!.log());
            await startRunner(`http://127.0.0.1:${relayAddress.port}`);
            const created = await createRun(
              recoveryThread,
              "e2e_agent",
              "Create exactly one note despite a runner crash",
            );
            const saved = await waitFor("native receipt committed before runner crash", async () => {
              if (relayError) throw relayError;
              return committed ?? false;
            });
            assert.equal(saved.runId, created.run_id);
            assert.equal(saved.receipt.completed.run_id, created.run_id);
            const unfinished = await request(`/threads/${recoveryThread}/runs/${created.run_id}`);
            assert.notEqual(unfinished.status, "success", "the crashed runner never confirmed completion");
            await startRunner();
            const final = await completedRun(recoveryThread, created.run_id);
            assert.equal(final.status, "success", JSON.stringify(final));
            assert.ok(final.metadata._runkite_attempt_generation >= 2);
            assert.ok(final.metadata._runkite_completion_generation >= 2);
            assert.equal(final.output.ai_sdk.generation, final.metadata._runkite_completion_generation);
            assert.equal(final.output.usage.prompt_tokens, 20);
            assert.equal(final.output.usage.completion_tokens, 4);
            const calls = (await readJsonLines(callsFile)).filter((call) => call.runId === created.run_id);
            const effects = (await readJsonLines(effectsFile)).filter((effect) => effect.runId === created.run_id);
            assert.equal(calls.length, 2, "receipt replay must not invoke either model step again");
            assert.equal(effects.length, 1, "receipt replay must not repeat the native tool");
            await waitFor("recovered turn's settled usage", async () => {
              assert.deepEqual(usageTotals(await request("/admin-api/usage/summary?agent_id=e2e_agent")), {
                runs: beforeUsage.runs + 1,
                input: beforeUsage.input + 20,
                output: beforeUsage.output + 4,
              });
              return true;
            });
            const thread = await request(`/threads/${recoveryThread}`);
            assert.deepEqual(thread.values, final.output);
          } finally {
            relay.closeAllConnections();
            await new Promise<void>((resolve, reject) => relay.close((error) => (error ? reject(error) : resolve())));
          }
        },
      );
    } catch (error) {
      t.diagnostic(logs());
      throw error;
    }
  },
);
