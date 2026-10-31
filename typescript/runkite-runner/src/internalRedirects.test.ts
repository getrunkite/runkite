import assert from "node:assert/strict";
import { once } from "node:events";
import http from "node:http";
import { test } from "node:test";
import { callAgent } from "./a2a.js";
import { AiSdkState } from "./aiSdkState.js";
import { getConnectorSession, proxyConnectorMcp } from "./connectors.js";
import { ProxyCheckpointSaver } from "./proxyCheckpoint.js";
import { shouldSkipRun } from "./runStatus.js";
import { RunkiteStore } from "./store.js";
import { RunkiteVectorStore } from "./vectorstore.js";

async function listen(server: http.Server): Promise<string> {
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert.ok(address && typeof address === "object");
  return `http://127.0.0.1:${address.port}`;
}

async function close(server: http.Server): Promise<void> {
  server.closeAllConnections();
  await new Promise<void>((resolve, reject) => server.close((error) => (error ? reject(error) : resolve())));
}

test("internal clients never forward runner credentials or state through redirects", async () => {
  let leaked = 0;
  let originRequests = 0;
  const destination = http.createServer((req, res) => {
    leaked++;
    req.resume();
    res.setHeader("Content-Type", "application/json");
    res.end("{}");
  });
  const target = await listen(destination);
  const origin = http.createServer((req, res) => {
    originRequests++;
    req.resume();
    res.writeHead(307, { Location: `${target}/untrusted` });
    res.end();
  });
  const base = await listen(origin);
  const savedToken = process.env.RUNNER_TOKEN;
  process.env.RUNNER_TOKEN = "redirect-test-runner-token";
  const config = { configurable: { run_id: "r", generation: 1 } };
  const state = new AiSdkState(base, "typescript-ai-sdk", "redirect-test-runner-token");
  const checkpoint = new ProxyCheckpointSaver({ httpBaseUrl: base, runnerToken: "redirect-test-runner-token" });
  const store = new RunkiteStore({ httpBaseUrl: base, runnerToken: "redirect-test-runner-token" });
  const vector = new RunkiteVectorStore(
    { embedDocuments: async () => [], embedQuery: async () => [] },
    {
      httpBaseUrl: base,
      namespace: "test",
      runnerToken: "redirect-test-runner-token",
    },
  );
  try {
    for (const operation of [
      () => getConnectorSession(config, "test", { controlPlaneUrl: base }),
      () =>
        proxyConnectorMcp(
          config,
          "test",
          { method: "tools/call" },
          { sessionToken: "capability", controlPlaneUrl: base },
        ),
      () => callAgent(config, "child", {}, { controlPlaneUrl: base, wait: false }),
      () => state.load("thread"),
      () => state.save("thread", { messages: [] }, null),
      () => checkpoint.getTuple({ configurable: { thread_id: "thread", checkpoint_id: "checkpoint" } }),
      () => store.batch([{ namespace: ["test"], key: "key" }]),
      () => vector.addVectors([[1]], [{ pageContent: "document", metadata: {}, id: "doc" }]),
    ]) {
      await assert.rejects(operation);
    }
    // This lookup intentionally fails open on transport failure. It still must
    // not disclose its token to a redirect target.
    assert.equal(await shouldSkipRun(base, "run", { runnerToken: "redirect-test-runner-token" }), false);
    assert.equal(originRequests, 9);
    assert.equal(leaked, 0);
  } finally {
    if (savedToken === undefined) delete process.env.RUNNER_TOKEN;
    else process.env.RUNNER_TOKEN = savedToken;
    await store.close();
    await close(origin);
    await close(destination);
  }
});
