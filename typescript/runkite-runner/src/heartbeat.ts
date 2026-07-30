/**
 * Runner-side heartbeat loop.
 * TypeScript mirror of the Python runner's heartbeat.py -- see that
 * module's doc comment for the full rationale.
 *
 * Before this, the control plane only knew a job was alive up to its
 * first StreamEvents message -- confirmed live at roughly 15ms after
 * dequeue, via a Redis MONITOR trace. A runner crash any time after that
 * left the run permanently stuck (queue=0, in-flight=0, no automatic
 * recovery), because nothing was watching it for the rest of its
 * execution.
 *
 * This closes that gap from the runner side: heartbeatLoop calls the
 * control plane's Heartbeat RPC every intervalMs while a job is actively
 * executing, extending the same in-flight lease Dequeue creates (see
 * bridge/server.go's Heartbeat handler and transport.JobQueue.Renew) for
 * the run's WHOLE duration, not just its first event. Started as its own
 * background loop right after a job is dequeued (worker.ts's handleJob
 * does this before streamEvents' first message even lands) and stopped
 * once the job finishes -- a runner that crashes simply stops
 * heartbeating, and the control plane's existing ReclaimStale reaper
 * (cmd/serve.go) picks it up after its normal max-age, exactly as it
 * already does for the dequeue-to-first-event window. No new reclaim
 * mechanism, just a mechanism to keep resetting the clock.
 */
import type { Metadata } from "@grpc/grpc-js";
import type { RunnerServiceClient } from "./proto.js";

// Matches cmd/serve.go's reaper ticker cadence (2s) and the max-age (6s)
// it reclaims stale jobs past -- see this module's doc comment: a live
// runner heartbeating at this interval never gets within one missed beat
// of that cutoff, while a crashed one (zero heartbeats, not just a slow
// one) reliably does.
export const DEFAULT_HEARTBEAT_INTERVAL_MS = 2000;

/** Handle returned by startHeartbeatLoop -- call stop() once the job
 * finishes to end the loop, mirroring the Python runner's
 * asyncio.Task.cancel() + await pattern. */
export interface HeartbeatHandle {
  stop(): void;
}

/**
 * Starts calling Heartbeat(runId) every intervalMs until stop() is
 * called. A failed heartbeat RPC is logged and swallowed, not thrown --
 * this loop is a liveness signal, not part of the run's own correctness:
 * a few missed heartbeats just mean the job might get reclaimed a bit
 * earlier than a perfectly-tuned system would (the residual "reclaimed
 * but original runner finishes anyway" edge case is Problem 3's fencing
 * token, not yet built, not this loop's job to prevent).
 */
export function startHeartbeatLoop(
  client: RunnerServiceClient,
  runId: string,
  metadata: Metadata,
  intervalMs: number = DEFAULT_HEARTBEAT_INTERVAL_MS,
): HeartbeatHandle {
  let stopped = false;
  let timer: ReturnType<typeof setTimeout> | undefined;

  const tick = async (): Promise<void> => {
    if (stopped) return;
    try {
      await new Promise<void>((resolve) => {
        client.heartbeat({ runId }, metadata, (err) => {
          if (err) console.warn(`Heartbeat RPC failed for run ${runId}:`, err.message);
          resolve(); // never rejects the outer promise -- see doc comment
        });
      });
    } catch (err) {
      console.warn(`Unexpected heartbeat error for run ${runId}:`, err);
    }
    if (!stopped) {
      timer = setTimeout(() => {
        void tick();
      }, intervalMs);
    }
  };

  timer = setTimeout(() => {
    void tick();
  }, intervalMs);

  return {
    stop(): void {
      stopped = true;
      if (timer) clearTimeout(timer);
    },
  };
}
