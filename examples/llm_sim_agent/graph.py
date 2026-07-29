"""Simulated-LLM-latency benchmark agent (bench/REPORT.md finding 1d's
own flagged follow-up: "a genuinely rigorous before/after benchmark for
the [--concurrency] workload would use an agent with an artificial
async sleep standing in for LLM latency, not echo_agent").

echo_agent is deliberately near-zero-compute, which isolates the
control plane's own overhead -- useful for that purpose, but it means
--concurrency's actual value proposition (overlapping many jobs'
*waiting* time, not their CPU time) was only ever proven with a burst
test (20 concurrent runs, ~14x speedup), not a sustained-load one.

`await asyncio.sleep(LLM_SIM_DELAY_MS)` stands in for one LLM call.
Deliberately a sleep, not a real API call: deterministic, free, no
external dependency, no rate limits/retries/non-determinism -- and a
sleep IS what a real LLM call looks like from the runner's own
perspective (an awaited I/O wait, not CPU work), so it exercises
exactly the mechanism `--concurrency` (asyncio.Task fan-out) is
supposed to help with, without any of the noise a real API call would
add to a benchmark meant to be re-run repeatedly.
"""

import asyncio
import os
from typing import Annotated, TypedDict

from langgraph.graph import StateGraph, START, END

# Default matches a fast, typical single-turn LLM response latency
# (e.g. a small/fast model, short completion) -- not a worst case, and
# not a multi-second reasoning-model response; deliberately the
# unglamorous common case this feature actually targets.
LLM_SIM_DELAY_MS = int(os.environ.get("LLM_SIM_DELAY_MS", "800"))


class State(TypedDict):
    messages: Annotated[list[dict], lambda a, b: a + b]


async def simulated_llm_call(state: State) -> State:
    await asyncio.sleep(LLM_SIM_DELAY_MS / 1000)
    last_message = state["messages"][-1]
    return {
        "messages": [{"role": "ai", "content": f"(simulated {LLM_SIM_DELAY_MS}ms LLM response to: {last_message['content']})"}]
    }


builder = StateGraph(State)
builder.add_node("llm_call", simulated_llm_call)
builder.add_edge(START, "llm_call")
builder.add_edge("llm_call", END)

graph = builder.compile()
