/**
 * HTTP round-trip self-check for ProxyCheckpointSaver.
 *
 * Exercises put / getTuple / list / putWrites against an in-memory
 * node:http mock that speaks the control-plane /internal/checkpoints
 * contract — no live CP required. Also asserts tenant-prefixed
 * configurable.thread_id is stripped before the path (storageThreadId)
 * and that list(filter) / putWrites-without-id fail loud.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { Checkpoint, CheckpointMetadata } from "@langchain/langgraph-checkpoint";
import { ProxyCheckpointSaver } from "./proxyCheckpoint.js";
import { resolveCheckpointHttpUrl } from "./checkpoint.js";
import { runWithBinding, runWithTenant } from "./tenantCtx.js";

type StoreKey = string; // `${threadId}\0${checkpointId}`

function makeMockServer(): {
  server: http.Server;
  store: Map<StoreKey, Buffer>;
  baseUrl: () => string;
} {
  const store = new Map<StoreKey, Buffer>();
  const meta = new Map<StoreKey, { checkpoint_id: string; framework: string; version: number }>();
  const versions = new Map<StoreKey, number>();

  const server = http.createServer((req, res) => {
    const host = req.headers.host || "127.0.0.1";
    const url = new URL(req.url || "/", `http://${host}`);
    const prefix = "/internal/checkpoints/";
    if (!url.pathname.startsWith(prefix)) {
      res.writeHead(404);
      res.end("not found");
      return;
    }
    const rest = decodeURIComponent(url.pathname.slice(prefix.length));
    const slash = rest.indexOf("/");
    const threadId = slash < 0 ? rest : rest.slice(0, slash);
    const checkpointId = slash < 0 ? undefined : rest.slice(slash + 1);

    const readBody = (): Promise<Buffer> =>
      new Promise((resolve, reject) => {
        const chunks: Buffer[] = [];
        req.on("data", (c) => chunks.push(Buffer.isBuffer(c) ? c : Buffer.from(c)));
        req.on("end", () => resolve(Buffer.concat(chunks)));
        req.on("error", reject);
      });

    void (async () => {
      if (req.method === "GET" && checkpointId === "latest") {
        const ns = url.searchParams.get("ns") || "";
        // Match CP ordering: checkpoint_id DESC, not Map insertion order.
        const candidates = [...store.keys()].sort((a, b) => {
          const ca = a.split("\0")[1] || "";
          const cb = b.split("\0")[1] || "";
          return cb.localeCompare(ca);
        });
        for (const k of candidates) {
          const [tid, cid] = k.split("\0") as [string, string];
          if (tid !== threadId) continue;
          const keyNs = cid.includes("\x1f") ? cid.slice(0, cid.indexOf("\x1f")) : "";
          if (keyNs !== ns) continue;
          const blob = store.get(k)!;
          const ver = versions.get(k) || 1;
          res.writeHead(200, {
            ETag: `"${ver}"`,
            "X-Runkite-Checkpoint-Id": encodeURIComponent(cid),
            "X-Runkite-Checkpoint-Framework": meta.get(k)?.framework || "",
          });
          res.end(blob);
          return;
        }
        res.writeHead(404);
        res.end("missing");
        return;
      }

      if (req.method === "PUT" && checkpointId) {
        const key: StoreKey = `${threadId}\0${checkpointId}`;
        const ifNone = (req.headers["if-none-match"] || "").toString().trim();
        if (ifNone === "*" && store.has(key)) {
          res.writeHead(412);
          res.end("already exists");
          return;
        }
        const ifMatch = (req.headers["if-match"] || "").toString().trim().replaceAll('"', "");
        const cur = versions.get(key);
        if (ifMatch && cur !== undefined && ifMatch !== String(cur)) {
          res.writeHead(412);
          res.end("version mismatch");
          return;
        }
        const body = await readBody();
        store.set(key, body);
        const newVer = (cur || 0) + 1;
        versions.set(key, newVer);
        meta.set(key, {
          checkpoint_id: checkpointId,
          framework: (req.headers["x-runkite-checkpoint-framework"] || "").toString(),
          version: newVer,
        });
        res.writeHead(204, { ETag: `"${newVer}"` });
        res.end();
        return;
      }

      if (req.method === "GET" && checkpointId) {
        const blob = store.get(`${threadId}\0${checkpointId}`);
        if (!blob) {
          res.writeHead(404);
          res.end("missing");
          return;
        }
        const ver = versions.get(`${threadId}\0${checkpointId}`) || 1;
        res.writeHead(200, { ETag: `"${ver}"` });
        res.end(blob);
        return;
      }

      if (req.method === "GET" && checkpointId === undefined) {
        const items = [...meta.entries()]
          .filter(([k]) => k.startsWith(`${threadId}\0`))
          .sort((a, b) => (b[1].checkpoint_id || "").localeCompare(a[1].checkpoint_id || ""))
          .map(([, m]) => m);
        const limit = parseInt(url.searchParams.get("limit") || "1000", 10);
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify(items.slice(0, limit)));
        return;
      }

      if (req.method === "DELETE" && checkpointId) {
        const key: StoreKey = `${threadId}\0${checkpointId}`;
        store.delete(key);
        meta.delete(key);
        versions.delete(key);
        res.writeHead(204);
        res.end();
        return;
      }

      res.writeHead(405);
      res.end("method not allowed");
    })().catch((err) => {
      res.writeHead(500);
      res.end(String(err));
    });
  });

  return {
    server,
    store,
    baseUrl: () => {
      const addr = server.address();
      if (!addr || typeof addr === "string") throw new Error("server not listening");
      return `http://127.0.0.1:${addr.port}`;
    },
  };
}

function listen(server: http.Server): Promise<void> {
  return new Promise((resolve) => server.listen(0, "127.0.0.1", () => resolve()));
}

function closeServer(server: http.Server): Promise<void> {
  return new Promise((resolve, reject) => server.close((err) => (err ? reject(err) : resolve())));
}

function checkpoint(cid: string, values?: Record<string, unknown>): Checkpoint {
  return {
    v: 1,
    id: cid,
    ts: "2026-01-01T00:00:00.000Z",
    channel_values: values ?? { messages: ["hi"] },
    channel_versions: { messages: 1 },
    versions_seen: {},
  };
}

function metadata(step: number): CheckpointMetadata {
  return { source: "loop", step, parents: {} };
}

test("proxy put+getTuple round-trip strips storageThreadId", async () => {
  const mock = makeMockServer();
  await listen(mock.server);
  const saver = new ProxyCheckpointSaver({ httpBaseUrl: mock.baseUrl(), runnerToken: "tok" });
  try {
    await runWithBinding({ tenantId: "acme", runId: "run-1", generation: 1 }, async () => {
      const config = { configurable: { thread_id: "acme:thr-1", checkpoint_ns: "" } };
      const out = await saver.put(config, checkpoint("cp-a"), metadata(1), {});
      assert.equal(out.configurable?.checkpoint_id, "cp-a");
      assert.equal(out.configurable?.thread_id, "acme:thr-1");
      assert.ok(mock.store.has("thr-1\0cp-a"), "HTTP path used bare thread_id");
      assert.ok(!mock.store.has("acme:thr-1\0cp-a"), "tenant prefix not used as storage key");

      const got = await saver.getTuple({
        configurable: { thread_id: "acme:thr-1", checkpoint_ns: "", checkpoint_id: "cp-a" },
      });
      assert.ok(got);
      assert.deepEqual(got!.checkpoint.channel_values.messages, ["hi"]);
      assert.equal(got!.config.configurable?.thread_id, "acme:thr-1");
    });
  } finally {
    await saver.close();
    await closeServer(mock.server);
  }
});

test("putWrites before put creates shell; create-only preserves shell", async () => {
  const mock = makeMockServer();
  await listen(mock.server);
  const saver = new ProxyCheckpointSaver({ httpBaseUrl: mock.baseUrl(), runnerToken: "tok" });
  try {
    await runWithTenant("default", async () => {
      const writeCfg = {
        configurable: { thread_id: "thr-shell", checkpoint_ns: "", checkpoint_id: "cp-missing" },
      };
      await saver.putWrites(writeCfg, [["messages", "orphan"]], "t-404");
      const pre = await saver.getTuple(writeCfg);
      assert.ok(pre?.pendingWrites?.some((w) => w[0] === "t-404"));

      await saver.put(
        { configurable: { thread_id: "thr-shell", checkpoint_ns: "" } },
        checkpoint("cp-missing", { messages: ["from-put"] }),
        metadata(1),
        {},
      );
      const after = await saver.getTuple(writeCfg);
      assert.ok(after);
      assert.deepEqual(after!.checkpoint.channel_values.messages, ["from-put"]);
      assert.ok(
        after!.pendingWrites?.some((w) => w[0] === "t-404"),
        "shell writes preserved",
      );
    });
  } finally {
    await saver.close();
    await closeServer(mock.server);
  }
});

test("latest header returns newest checkpoint", async () => {
  const mock = makeMockServer();
  await listen(mock.server);
  const saver = new ProxyCheckpointSaver({ httpBaseUrl: mock.baseUrl() });
  try {
    await runWithTenant("default", async () => {
      const cfg = { configurable: { thread_id: "thr-latest", checkpoint_ns: "" } };
      await saver.put(cfg, checkpoint("cp-a"), metadata(1), {});
      await saver.put(
        { configurable: { ...cfg.configurable, checkpoint_id: "cp-a" } },
        checkpoint("cp-b", { messages: ["bye"] }),
        metadata(2),
        {},
      );
      const latest = await saver.getTuple(cfg);
      assert.equal(latest?.checkpoint.id, "cp-b");

      const listed: string[] = [];
      for await (const item of saver.list(cfg, { limit: 10 })) {
        listed.push(item.checkpoint.id);
      }
      assert.deepEqual(listed, ["cp-b", "cp-a"]);
    });
  } finally {
    await saver.close();
    await closeServer(mock.server);
  }
});

test("list(filter) throws; putWrites without checkpoint_id throws", async () => {
  const mock = makeMockServer();
  await listen(mock.server);
  const saver = new ProxyCheckpointSaver({ httpBaseUrl: mock.baseUrl() });
  try {
    await runWithTenant("default", async () => {
      await assert.rejects(async () => {
        for await (const _ of saver.list({ configurable: { thread_id: "thr-x" } }, { filter: { source: "loop" } })) {
          // drain
        }
      }, /filter/);
      await assert.rejects(
        () =>
          saver.putWrites(
            { configurable: { thread_id: "thr-x", checkpoint_ns: "" } },
            [["messages", "orphan"]],
            "t-missing",
          ),
        /checkpoint_id/,
      );
    });
  } finally {
    await saver.close();
    await closeServer(mock.server);
  }
});

test("put/putWrites soft-no-op on run_not_inflight", async () => {
  const server = http.createServer((_req, res) => {
    res.writeHead(403, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: "run_not_inflight" }));
  });
  await listen(server);
  const addr = server.address();
  if (!addr || typeof addr === "string") throw new Error("no listen address");
  const saver = new ProxyCheckpointSaver({
    httpBaseUrl: `http://127.0.0.1:${addr.port}`,
    runnerToken: "tok",
  });
  try {
    await runWithTenant("default", async () => {
      const out = await saver.put(
        { configurable: { thread_id: "thr-x", checkpoint_ns: "" } },
        checkpoint("cp-dead"),
        metadata(1),
        {},
      );
      assert.equal(out.configurable?.checkpoint_id, "cp-dead");
      await saver.putWrites(
        {
          configurable: {
            thread_id: "thr-x",
            checkpoint_ns: "",
            checkpoint_id: "cp-dead",
          },
        },
        [["messages", "late"]],
        "t-late",
      );
    });
  } finally {
    await saver.close();
    await closeServer(server);
  }
});

test("put CAS retries once on 412 then succeeds", async () => {
  let puts = 0;
  const blobs = new Map<string, Buffer>();
  const vers = new Map<string, number>();
  const server = http.createServer((req, res) => {
    const host = req.headers.host || "127.0.0.1";
    const url = new URL(req.url || "/", `http://${host}`);
    const rest = decodeURIComponent(url.pathname.replace(/^\/internal\/checkpoints\//, ""));
    const slash = rest.indexOf("/");
    const threadId = slash < 0 ? rest : rest.slice(0, slash);
    const checkpointId = slash < 0 ? undefined : rest.slice(slash + 1);
    const key = `${threadId}\0${checkpointId}`;
    const chunks: Buffer[] = [];
    req.on("data", (c) => chunks.push(Buffer.isBuffer(c) ? c : Buffer.from(c)));
    req.on("end", () => {
      if (req.method === "GET" && checkpointId) {
        const blob = blobs.get(key);
        if (!blob) {
          // First GET is 404 → create-only PUT; force that PUT to 412 once.
          res.writeHead(404);
          res.end("missing");
          return;
        }
        res.writeHead(200, { ETag: `"${vers.get(key) || 1}"` });
        res.end(blob);
        return;
      }
      if (req.method === "PUT" && checkpointId) {
        puts += 1;
        if (puts === 1) {
          // Peer won create-only: return 412 so saver retries via GET+merge.
          blobs.set(key, Buffer.from("peer-shell"));
          vers.set(key, 1);
          res.writeHead(412);
          res.end("exists");
          return;
        }
        const body = Buffer.concat(chunks);
        blobs.set(key, body);
        const ver = (vers.get(key) || 0) + 1;
        vers.set(key, ver);
        res.writeHead(204, { ETag: `"${ver}"` });
        res.end();
        return;
      }
      res.writeHead(405);
      res.end();
    });
  });
  await listen(server);
  const addr = server.address();
  if (!addr || typeof addr === "string") throw new Error("no addr");
  const saver = new ProxyCheckpointSaver({
    httpBaseUrl: `http://127.0.0.1:${addr.port}`,
    runnerToken: "tok",
  });
  try {
    await runWithTenant("default", async () => {
      const out = await saver.put(
        { configurable: { thread_id: "thr-cas", checkpoint_ns: "" } },
        checkpoint("cp-cas", { messages: ["retried"] }),
        metadata(1),
        {},
      );
      assert.equal(out.configurable?.checkpoint_id, "cp-cas");
      assert.ok(puts >= 2, `expected CAS retry, puts=${puts}`);
    });
  } finally {
    await saver.close();
    await closeServer(server);
  }
});

test("cross-saver create-only race preserves putWrites shell", async () => {
  const mock = makeMockServer();
  await listen(mock.server);
  const s1 = new ProxyCheckpointSaver({ httpBaseUrl: mock.baseUrl(), runnerToken: "tok" });
  const s2 = new ProxyCheckpointSaver({ httpBaseUrl: mock.baseUrl(), runnerToken: "tok" });
  try {
    await runWithTenant("default", async () => {
      const writeCfg = {
        configurable: { thread_id: "thr-race", checkpoint_ns: "", checkpoint_id: "cp-race" },
      };
      await Promise.all([
        s1.putWrites(writeCfg, [["messages", "from-writes"]], "task-w"),
        s2.put(
          { configurable: { thread_id: "thr-race", checkpoint_ns: "" } },
          checkpoint("cp-race", { messages: ["from-put"] }),
          metadata(1),
          {},
        ),
      ]);
      const got = await s1.getTuple(writeCfg);
      assert.ok(got);
      assert.deepEqual(got!.checkpoint.channel_values.messages, ["from-put"]);
      assert.ok(
        got!.pendingWrites?.some((w) => w[0] === "task-w"),
        "shell writes preserved",
      );
    });
  } finally {
    await s1.close();
    await s2.close();
    await closeServer(mock.server);
  }
});

test("update refuses GET 200 without ETag", async () => {
  const server = http.createServer((req, res) => {
    if (req.method === "GET") {
      // Valid empty-ish envelope is unnecessary — decode failure yields writes=[].
      // Critical: 200 without ETag must not become unconditional PUT.
      res.writeHead(200, { "Content-Type": "application/octet-stream" });
      res.end(Buffer.from([0]));
      return;
    }
    res.writeHead(500);
    res.end("should not PUT");
  });
  await listen(server);
  const addr = server.address();
  if (!addr || typeof addr === "string") throw new Error("no addr");
  const saver = new ProxyCheckpointSaver({
    httpBaseUrl: `http://127.0.0.1:${addr.port}`,
    runnerToken: "tok",
  });
  try {
    await runWithTenant("default", async () => {
      await assert.rejects(
        () =>
          saver.put({ configurable: { thread_id: "thr-x", checkpoint_ns: "" } }, checkpoint("cp-x"), metadata(1), {}),
        /no ETag/,
      );
    });
  } finally {
    await saver.close();
    await closeServer(server);
  }
});

test("resolveCheckpointHttpUrl prefers RUNKITE_HTTP_URL including empty", () => {
  const prev = process.env.RUNKITE_HTTP_URL;
  try {
    process.env.RUNKITE_HTTP_URL = " http://cp.example ";
    assert.equal(resolveCheckpointHttpUrl("http://fallback"), "http://cp.example");
    process.env.RUNKITE_HTTP_URL = "  ";
    assert.equal(resolveCheckpointHttpUrl("http://fallback"), undefined);
    delete process.env.RUNKITE_HTTP_URL;
    assert.equal(resolveCheckpointHttpUrl(" http://fallback "), "http://fallback");
    assert.equal(resolveCheckpointHttpUrl("  "), undefined);
  } finally {
    if (prev === undefined) delete process.env.RUNKITE_HTTP_URL;
    else process.env.RUNKITE_HTTP_URL = prev;
  }
});
