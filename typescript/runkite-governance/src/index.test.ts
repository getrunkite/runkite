import assert from "node:assert/strict";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { once } from "node:events";
import { test } from "node:test";
import { createGovernanceClient, GovernanceError } from "./index.ts";

type Hit = { method: string; path: string; body: unknown; auth: string };

async function fakePlane(
  handle: (req: IncomingMessage, res: ServerResponse, hits: Hit[]) => void,
): Promise<{ url: string; hits: Hit[]; close: () => Promise<void> }> {
  const hits: Hit[] = [];
  const server = createServer(async (req, res) => {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk as Buffer);
    const text = Buffer.concat(chunks).toString("utf8");
    let body: unknown = undefined;
    if (text) body = JSON.parse(text);
    hits.push({
      method: req.method ?? "",
      path: req.url ?? "",
      body,
      auth: String(req.headers.authorization ?? ""),
    });
    handle(req, res, hits);
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const addr = server.address();
  if (!addr || typeof addr === "string") throw new Error("no port");
  return {
    url: `http://127.0.0.1:${addr.port}`,
    hits,
    close: () =>
      new Promise((resolve, reject) => {
        server.close((err) => (err ? reject(err) : resolve()));
      }),
  };
}

function json(res: ServerResponse, status: number, body: unknown) {
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(JSON.stringify(body));
}

const call = {
  agentId: "partner",
  connector: "workspace",
  tool: "uploadFile",
  args: { name: "a.txt" },
  runId: "turn-1",
};

test("decide posts the call and returns allow without polling", async () => {
  const plane = await fakePlane((_req, res) => {
    json(res, 200, { effect: "allow", reason: "grant matched", reason_code: "grant" });
  });
  try {
    const client = createGovernanceClient({ baseUrl: plane.url, apiKey: "rk_test" });
    const decision = await client.decide(call);
    assert.deepEqual(decision, { effect: "allow", reason: "grant matched", reasonCode: "grant" });
    assert.equal(plane.hits.length, 1);
    assert.equal(plane.hits[0].method, "POST");
    assert.equal(plane.hits[0].path, "/governance/decide");
    assert.equal(plane.hits[0].auth, "Bearer rk_test");
    assert.deepEqual(plane.hits[0].body, {
      agent_id: "partner",
      connector: "workspace",
      tool: "uploadFile",
      args: { name: "a.txt" },
      run_id: "turn-1",
    });
  } finally {
    await plane.close();
  }
});

test("resolve polls until approved and maps that to allow", async () => {
  let polls = 0;
  const plane = await fakePlane((req, res) => {
    if (req.url === "/governance/decide") {
      json(res, 200, { effect: "pending", reason: "hold", reason_code: "mandatory_hitl", action_id: "pending-1" });
      return;
    }
    polls += 1;
    if (polls === 1) {
      json(res, 200, { action_id: "pending-1", status: "pending", reason: "hold", reason_code: "mandatory_hitl" });
      return;
    }
    json(res, 200, { action_id: "pending-1", status: "approved", reason: "ok", reason_code: "approved" });
  });
  try {
    const client = createGovernanceClient({ baseUrl: plane.url + "/", intervalMs: 5, timeoutMs: 1000 });
    const outcome = await client.resolve(call);
    assert.deepEqual(outcome, {
      effect: "allow",
      reason: "ok",
      reasonCode: "approved",
      actionId: "pending-1",
      status: "approved",
    });
    assert.equal(plane.hits.filter((hit) => hit.method === "GET").length, 2);
  } finally {
    await plane.close();
  }
});

test("resolve maps rejected and consumed to deny", async () => {
  const plane = await fakePlane((req, res, hits) => {
    if (req.url === "/governance/decide") {
      const id = hits.length === 1 ? "pending-no" : "pending-used";
      json(res, 200, { effect: "pending", action_id: id });
      return;
    }
    if (req.url === "/governance/actions/pending-no") {
      json(res, 200, { action_id: "pending-no", status: "rejected", reason_code: "pending_expired" });
      return;
    }
    json(res, 200, { action_id: "pending-used", status: "consumed" });
  });
  try {
    const client = createGovernanceClient({ baseUrl: plane.url, intervalMs: 5, timeoutMs: 1000 });
    const rejected = await client.resolve(call);
    assert.equal(rejected.effect, "deny");
    assert.equal(rejected.status, "rejected");
    assert.equal(rejected.reasonCode, "pending_expired");
    const consumed = await client.resolve(call);
    assert.equal(consumed.effect, "deny");
    assert.equal(consumed.status, "consumed");
  } finally {
    await plane.close();
  }
});

test("resolve times out while the row stays pending", async () => {
  const plane = await fakePlane((req, res) => {
    if (req.url === "/governance/decide") {
      json(res, 200, { effect: "pending", action_id: "pending-wait" });
      return;
    }
    json(res, 200, { action_id: "pending-wait", status: "pending" });
  });
  try {
    const client = createGovernanceClient({ baseUrl: plane.url, intervalMs: 50, timeoutMs: 20 });
    await assert.rejects(() => client.resolve(call), (err: unknown) => {
      assert.ok(err instanceof GovernanceError);
      assert.equal(err.status, 408);
      return true;
    });
  } finally {
    await plane.close();
  }
});

test("non-2xx surfaces the plane message", async () => {
  const plane = await fakePlane((_req, res) => {
    json(res, 403, { message: "governance:decide permission required" });
  });
  try {
    const client = createGovernanceClient({ baseUrl: plane.url });
    await assert.rejects(() => client.decide(call), (err: unknown) => {
      assert.ok(err instanceof GovernanceError);
      assert.equal(err.status, 403);
      assert.equal(err.message, "governance:decide permission required");
      return true;
    });
  } finally {
    await plane.close();
  }
});
