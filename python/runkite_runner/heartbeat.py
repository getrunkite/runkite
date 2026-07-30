"""Runner-side heartbeat loop.

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

Also carries item 16, Problem 3's fencing token (generation): this
runner's transient connectivity blip might make it miss the reaper's
max-age window, get reclaimed, and replaced by a second runner -- but if
the blip was genuinely transient, THIS runner is still executing and
doesn't know any of that happened yet. The next Heartbeat call after a
reclaim gets back superseded=True, which is this runner's actionable
signal to stop: it sets cancel_event, the SAME event a real WatchCancels
cancel signal would set, so execute_run's existing cooperative-
cancellation path (checked every streamed chunk in worker.py, raced via
run_cancellable in generic_worker.py's single-call adapters) takes over
without this module needing its own separate stopping mechanism.
"""

from __future__ import annotations

import asyncio
import logging

import grpc

from . import runner_pb2, runner_pb2_grpc

logger = logging.getLogger("runkite.runner")

# Matches cmd/serve.go's reaper ticker cadence (2s) and the max-age (6s)
# it reclaims stale jobs past -- see this module's docstring: a live
# runner heartbeating at this interval never gets within one missed beat
# of that cutoff, while a crashed one (zero heartbeats, not just a slow
# one) reliably does.
DEFAULT_HEARTBEAT_INTERVAL_S = 2.0


async def heartbeat_loop(
    stub: runner_pb2_grpc.RunnerServiceStub,
    run_id: str,
    auth_metadata: list,
    generation: int = 0,
    cancel_event: asyncio.Event | None = None,
    interval_s: float = DEFAULT_HEARTBEAT_INTERVAL_S,
) -> None:
    """Calls Heartbeat(run_id, generation) every interval_s until
    cancelled.

    Intended to run as its own asyncio.Task, cancelled by the caller once
    the job finishes (success, error, or interruption) -- see worker.py/
    generic_worker.py's _handle_job. A failed heartbeat RPC is logged and
    swallowed, not raised: this loop is a liveness signal, not part of
    the run's own correctness -- a few missed heartbeats just mean the
    job might get reclaimed a bit earlier than a perfectly-tuned system
    would.

    generation defaults to 0 (unfenced -- matches a pre-fencing control
    plane, or a caller that doesn't track it) so existing callers don't
    have to change to keep working. If the response comes back
    superseded=True -- a newer generation has already been dispatched,
    meaning this runner was reclaimed while genuinely still executing --
    this sets cancel_event (if given) and stops looping: continuing to
    heartbeat a generation the control plane has already moved on from
    is pointless, and every subsequent call would just be rejected the
    same way.
    """
    while True:
        await asyncio.sleep(interval_s)
        try:
            resp = await stub.Heartbeat(
                runner_pb2.HeartbeatRequest(run_id=run_id, generation=generation), metadata=auth_metadata
            )
            if resp.superseded:
                logger.warning(
                    f"Run {run_id} superseded (generation {generation} is stale) -- "
                    "signaling cancellation and stopping heartbeat"
                )
                if cancel_event is not None:
                    cancel_event.set()
                return
        except grpc.aio.AioRpcError as e:
            logger.warning(f"Heartbeat RPC failed for run {run_id}: {e.code()} {e.details()}")
        except Exception:
            logger.exception(f"Unexpected heartbeat error for run {run_id}")
