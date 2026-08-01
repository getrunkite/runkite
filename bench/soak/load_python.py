#!/usr/bin/env python3
"""Python LangGraph SDK soak client — mixed agents, concurrent workers."""

from __future__ import annotations

import argparse
import asyncio
import random
import time
from collections import Counter

from langgraph_sdk import get_client

AGENTS = [
    ("echo_agent", 0.45),
    ("llm_sim_agent", 0.35),
    ("slow_agent", 0.10),
    ("store_agent", 0.10),
]


def pick_agent() -> str:
    r = random.random()
    acc = 0.0
    for name, w in AGENTS:
        acc += w
        if r <= acc:
            return name
    return AGENTS[0][0]


def classify_result(agent: str, result) -> str:
    status = None
    if isinstance(result, dict):
        run = result.get("run")
        if isinstance(run, dict):
            status = run.get("status")
        status = status or result.get("status")
    if status in (None, "success"):
        return f"ok:{agent}"
    return f"bad_status:{agent}:{status}"


async def worker(name: str, url: str, stop_at: float, stats: Counter, lock: asyncio.Lock) -> None:
    client = get_client(url=url)
    while time.time() < stop_at:
        agent = pick_agent()
        t0 = time.monotonic()
        try:
            thread = await client.threads.create()
            tid = thread["thread_id"] if isinstance(thread, dict) else thread.thread_id
            result = await client.runs.wait(
                tid,
                agent,
                input={"messages": [{"role": "user", "content": f"soak-{name}-{time.time():.0f}"}]},
            )
            key = classify_result(agent, result)
        except Exception as e:
            key = f"err:{agent}:{type(e).__name__}"
        dt = time.monotonic() - t0
        async with lock:
            stats[key] += 1
            stats["cycles"] += 1
            stats["latency_ms_sum"] += int(dt * 1000)


async def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://127.0.0.1:2026")
    ap.add_argument("--workers", type=int, default=16)
    ap.add_argument("--duration", type=int, default=1800)
    args = ap.parse_args()

    stop_at = time.time() + args.duration
    stats: Counter = Counter()
    lock = asyncio.Lock()
    print(f"python sdk soak: workers={args.workers} duration={args.duration}s url={args.url}", flush=True)
    tasks = [asyncio.create_task(worker(f"py{i}", args.url, stop_at, stats, lock)) for i in range(args.workers)]

    while time.time() < stop_at:
        await asyncio.sleep(15)
        async with lock:
            cyc = stats["cycles"]
            avg = (stats["latency_ms_sum"] / cyc) if cyc else 0
            snap = {k: v for k, v in stats.items() if k not in ("latency_ms_sum",)}
            print(f"[py] cycles={cyc} avg_ms={avg:.0f} snapshot={snap}", flush=True)

    await asyncio.gather(*tasks, return_exceptions=True)
    print(f"[py] FINAL {dict(stats)}", flush=True)


if __name__ == "__main__":
    asyncio.run(main())
