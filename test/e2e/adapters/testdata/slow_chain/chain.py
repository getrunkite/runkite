"""Test-only slow chain for the LangChain adapter's e2e coverage
(test/e2e/adapters) -- proves cancellation works through the REAL
control plane + a real langchain_adapter runner subprocess, not just the
unit-level run_cancellable/generic_worker mocks. Sleeps long enough
(~6s) that a cancel issued after ~2s reliably lands mid-execution,
mirroring examples/all_agents' slow_agent's same role for the LangGraph
runner's own VG-002 e2e test.
"""

import asyncio


class _SlowRunnable:
    async def ainvoke(self, input_dict):
        await asyncio.sleep(6)
        return f"finished (slowly) responding to: {input_dict.get('input', '')}"


chain = _SlowRunnable()
