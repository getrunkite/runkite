"""Runner-side heartbeat loop (plans/pending_items.md item 16, Problem 2).

Before this, the control plane only knew a job was alive up to its first
StreamEvents message -- confirmed live at roughly 15ms after dequeue, via
a Redis MONITOR trace. A runner crash any time after that left the run
permanently stuck (queue=0, in-flight=0, no automatic recovery), because
nothing was watching it for the rest of its execution.

This closes that gap from the runner side: heartbeat_loop calls the
control plane's Heartbeat RPC every interval_s while a job is actively
executing, extending the same in-flight lease Dequeue creates (see
bridge/server.go's Heartbeat handler and transport.JobQueue.Renew) for
the run's WHOLE duration, not just its first event. Started as its own
asyncio.Task right after a job is dequeued (both worker.py and
generic_worker.py do this in _handle_job, before StreamEvents' first
message even lands) and cancelled once the job finishes -- a runner that
crashes simply stops heartbeating, and the control plane's existing
ReclaimStale reaper (cmd/serve.go) picks it up after its normal max-age,
exactly as it already does for the dequeue-to-first-event window. No new
reclaim mechanism, just a mechanism to keep resetting the clock.
"""

from __future__ import annotations

import asyncio
import logging

import grpc

from . import runner_pb2
from . import runner_pb2_grpc

logger = logging.getLogger("runkite.runner")

# Matches cmd/serve.go's reaper ticker cadence (2s) and the max-age (6s)
# it reclaims stale jobs past -- see this module's docstring: a live
# runner heartbeating at this interval never gets within one missed beat
# of that cutoff, while a crashed one (zero heartbeats, not just a slow
# one) reliably does.
DEFAULT_HEARTBEAT_INTERVAL_S = 2.0


async def heartbeat_loop(
    stub: "runner_pb2_grpc.RunnerServiceStub",
    run_id: str,
    auth_metadata: list,
    interval_s: float = DEFAULT_HEARTBEAT_INTERVAL_S,
) -> None:
    """Calls Heartbeat(run_id) every interval_s until cancelled.

    Intended to run as its own asyncio.Task, cancelled by the caller once
    the job finishes (success, error, or interruption) -- see worker.py/
    generic_worker.py's _handle_job. A failed heartbeat RPC is logged and
    swallowed, not raised: this loop is a liveness signal, not part of
    the run's own correctness -- a few missed heartbeats just mean the
    job might get reclaimed a bit earlier than a perfectly-tuned system
    would (the residual "reclaimed but original runner finishes anyway"
    edge case is Problem 3's fencing token, not yet built, not this
    loop's job to prevent).
    """
    while True:
        await asyncio.sleep(interval_s)
        try:
            await stub.Heartbeat(runner_pb2.HeartbeatRequest(run_id=run_id), metadata=auth_metadata)
        except grpc.aio.AioRpcError as e:
            logger.warning(f"Heartbeat RPC failed for run {run_id}: {e.code()} {e.details()}")
        except Exception:
            logger.exception(f"Unexpected heartbeat error for run {run_id}")
