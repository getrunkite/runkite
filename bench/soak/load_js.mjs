#!/usr/bin/env node
/** JavaScript HTTP client soak — Agent Protocol create/wait (no real LLM). */

import { randomUUID } from "node:crypto";

const URL = process.env.SOAK_URL || "http://127.0.0.1:2026";
const WORKERS = Number(process.env.SOAK_JS_WORKERS || 12);
const DURATION = Number(process.env.SOAK_DURATION || 1800);

const AGENTS = [
  ["echo_agent", 0.5],
  ["llm_sim_agent", 0.35],
  ["store_agent", 0.15],
];

function pickAgent() {
  const r = Math.random();
  let acc = 0;
  for (const [name, w] of AGENTS) {
    acc += w;
    if (r <= acc) return name;
  }
  return AGENTS[0][0];
}

const stats = { cycles: 0, ok: 0, err: 0, latency_ms_sum: 0, by: {} };

async function oneCycle() {
  const agent = pickAgent();
  const threadId = `js-${Date.now()}-${randomUUID().slice(0, 8)}`;
  const t0 = performance.now();
  try {
    const create = await fetch(`${URL}/threads/${threadId}/runs`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        agent_id: agent,
        input: { messages: [{ role: "user", content: `soak-js-${Date.now()}` }] },
      }),
    });
    if (!create.ok) throw new Error(`create ${create.status}`);
    const { run_id } = await create.json();
    const wait = await fetch(`${URL}/threads/${threadId}/runs/${run_id}/wait`);
    if (!wait.ok) throw new Error(`wait ${wait.status}`);
    const body = await wait.json();
    const status = body?.run?.status || body?.status;
    if (status && status !== "success") throw new Error(`status ${status}`);
    stats.ok++;
    stats.by[agent] = (stats.by[agent] || 0) + 1;
  } catch (e) {
    stats.err++;
    stats.by[`err:${agent}`] = (stats.by[`err:${agent}`] || 0) + 1;
  } finally {
    stats.cycles++;
    stats.latency_ms_sum += performance.now() - t0;
  }
}

async function worker(stopAt) {
  while (Date.now() / 1000 < stopAt) {
    await oneCycle();
  }
}

const stopAt = Date.now() / 1000 + DURATION;
console.log(`js client soak: workers=${WORKERS} duration=${DURATION}s url=${URL}`);
const tasks = Array.from({ length: WORKERS }, () => worker(stopAt));

const ticker = setInterval(() => {
  const avg = stats.cycles ? stats.latency_ms_sum / stats.cycles : 0;
  console.log(`[js] cycles=${stats.cycles} ok=${stats.ok} err=${stats.err} avg_ms=${avg.toFixed(0)} by=${JSON.stringify(stats.by)}`);
}, 15000);

await Promise.all(tasks);
clearInterval(ticker);
console.log(`[js] FINAL`, stats);
