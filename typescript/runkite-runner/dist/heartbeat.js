import { logger } from "./logger.js";
// Matches cmd/serve.go's reaper ticker cadence (2s) and the max-age (6s)
// it reclaims stale jobs past -- see this module's doc comment: a live
// runner heartbeating at this interval never gets within one missed beat
// of that cutoff, while a crashed one (zero heartbeats, not just a slow
// one) reliably does.
export const DEFAULT_HEARTBEAT_INTERVAL_MS = 2000;
/**
 * Starts calling Heartbeat(runId) every intervalMs until stop() is
 * called. A failed heartbeat RPC is logged and swallowed, not thrown --
 * this loop is a liveness signal, not part of the run's own correctness:
 * a few missed heartbeats just mean the job might get reclaimed a bit
 * earlier than a perfectly-tuned system would (the residual "reclaimed
 * but original runner finishes anyway" edge case is Problem 3's fencing
 * token, not yet built, not this loop's job to prevent).
 */
export function startHeartbeatLoop(client, runId, metadata, intervalMs = DEFAULT_HEARTBEAT_INTERVAL_MS) {
    let stopped = false;
    let timer;
    const tick = async () => {
        if (stopped)
            return;
        try {
            await new Promise((resolve) => {
                client.heartbeat({ runId }, metadata, (err) => {
                    if (err)
                        logger.warn(`Heartbeat RPC failed for run ${runId}:`, err.message);
                    resolve(); // never rejects the outer promise -- see doc comment
                });
            });
        }
        catch (err) {
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
        stop() {
            stopped = true;
            if (timer)
                clearTimeout(timer);
        },
    };
}
