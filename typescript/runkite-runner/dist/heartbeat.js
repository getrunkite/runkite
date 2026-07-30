import { logger } from "./logger.js";
// Matches cmd/serve.go's reaper ticker cadence (2s) and the max-age (6s)
// it reclaims stale jobs past -- see this module's doc comment: a live
// runner heartbeating at this interval never gets within one missed beat
// of that cutoff, while a crashed one (zero heartbeats, not just a slow
// one) reliably does.
export const DEFAULT_HEARTBEAT_INTERVAL_MS = 2000;
/**
 * Starts calling Heartbeat(runId, generation) every intervalMs until
 * stop() is called (or a superseded response stops it early). A failed
 * heartbeat RPC is logged and swallowed, not thrown -- this loop is a
 * liveness signal, not part of the run's own correctness: a few missed
 * heartbeats just mean the job might get reclaimed a bit earlier than a
 * perfectly-tuned system would.
 */
export function startHeartbeatLoop(client, runId, metadata, options = {}) {
    const { generation = 0, onSuperseded, intervalMs = DEFAULT_HEARTBEAT_INTERVAL_MS } = options;
    let stopped = false;
    let timer;
    const tick = async () => {
        if (stopped)
            return;
        try {
            const superseded = await new Promise((resolve) => {
                client.heartbeat({ runId, generation: String(generation) }, metadata, (err, resp) => {
                    if (err) {
                        logger.warn(`Heartbeat RPC failed for run ${runId}:`, err.message);
                        resolve(false);
                    }
                    else {
                        resolve(resp.superseded);
                    }
                });
            });
            if (superseded) {
                logger.warn(`Run ${runId} superseded (generation ${generation} is stale) -- signaling cancellation and stopping heartbeat`);
                stopped = true;
                onSuperseded?.();
                return;
            }
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
