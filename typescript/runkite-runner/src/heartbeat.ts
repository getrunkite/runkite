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
 *
 * Also carries item 16, Problem 3's fencing token (generation): this
 * runner's transient connectivity blip might make it miss the reaper's
 * max-age window, get reclaimed, and replaced by a second runner -- but
 * if the blip was genuinely transient, THIS runner is still executing
 * and doesn't know any of that happened yet. The next Heartbeat call
 * after a reclaim gets back superseded=true, which is this runner's
 * actionable signal to stop: onSuperseded fires (worker.ts wires it to
 * the SAME cancelState a real WatchCancels cancel signal would set), so
 * executeRun's existing cooperative-cancellation check takes over
 * without this module needing its own separate stopping mechanism.
 */
import type { Metadata } from "@grpc/grpc-js";
import { logger } from "./logger.js";
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

export interface HeartbeatLoopOptions {
  /** Fencing token from this run's RunAssignment (see this module's own
   * doc comment) -- 0 (the default) is "unfenced," matching a control
   * plane that predates this field. */
  generation?: number;
  /** Fired the FIRST time a heartbeat comes back superseded=true, then
   * the loop stops itself -- see this module's own doc comment for why
   * continuing to heartbeat after that point is pointless. Deliberately
   * a plain callback, not a shared mutable object, to keep this module
   * decoupled from worker.ts's CancelState shape. */
  onSuperseded?: () => void;
  intervalMs?: number;
}

/**
 * Starts calling Heartbeat(runId, generation) every intervalMs until
 * stop() is called (or a superseded response stops it early). A failed
 * heartbeat RPC is logged and swallowed, not thrown -- this loop is a
 * liveness signal, not part of the run's own correctness: a few missed
 * heartbeats just mean the job might get reclaimed a bit earlier than a
 * perfectly-tuned system would.
 */
export function startHeartbeatLoop(
  client: RunnerServiceClient,
  runId: string,
  metadata: Metadata,
  options: HeartbeatLoopOptions = {},
): HeartbeatHandle {
  const { generation = 0, onSuperseded, intervalMs = DEFAULT_HEARTBEAT_INTERVAL_MS } = options;
  let stopped = false;
  let timer: ReturnType<typeof setTimeout> | undefined;

  const tick = async (): Promise<void> => {
    if (stopped) return;
    try {
      const superseded = await new Promise<boolean>((resolve) => {
        client.heartbeat({ runId, generation: String(generation) }, metadata, (err, resp) => {
          if (err) {
            logger.warn(`Heartbeat RPC failed for run ${runId}:`, err.message);
            resolve(false);
          } else {
            resolve(resp.superseded);
          }
        });
      });
      if (superseded) {
        logger.warn(
          `Run ${runId} superseded (generation ${generation} is stale) -- signaling cancellation and stopping heartbeat`,
        );
        stopped = true;
        onSuperseded?.();
        return;
      }
    } catch (err) {
      logger.warn(`Unexpected heartbeat error for run ${runId}:`, err);
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
