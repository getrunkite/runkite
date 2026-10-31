import { logger } from "./logger.js";
// Matches cmd/serve.go's reaper ticker cadence (2s) and the max-age (6s)
// it reclaims stale jobs past -- see this module's doc comment: a live
// runner heartbeating at this interval never gets within one missed beat
// of that cutoff, while a crashed one (zero heartbeats, not just a slow
// one) reliably does.
export const DEFAULT_HEARTBEAT_INTERVAL_MS = 2000;
export const DEFAULT_HEARTBEAT_RPC_TIMEOUT_MS = 1000;
/**
 * Starts calling Heartbeat(runId, generation) every intervalMs until
 * stop() is called (or a superseded response stops it early). A failed
 * heartbeat RPC is logged and swallowed, not thrown -- this loop is a
 * liveness signal, not part of the run's own correctness: a few missed
 * heartbeats just mean the job might get reclaimed a bit earlier than a
 * perfectly-tuned system would.
 */
export function startHeartbeatLoop(client, runId, metadata, options = {}) {
    const { generation = 0, onSuperseded, intervalMs = DEFAULT_HEARTBEAT_INTERVAL_MS, rpcTimeoutMs = DEFAULT_HEARTBEAT_RPC_TIMEOUT_MS, } = options;
    if (!Number.isFinite(rpcTimeoutMs) || rpcTimeoutMs <= 0 || rpcTimeoutMs > 2_147_483_647)
        throw new RangeError("heartbeat rpcTimeoutMs must be positive and at most 2147483647");
    let stopped = false;
    let timer;
    let cancelPending;
    const tick = async () => {
        if (stopped)
            return;
        try {
            const superseded = await new Promise((resolve) => {
                let settled = false;
                let call;
                const finish = (value) => {
                    if (settled)
                        return;
                    settled = true;
                    clearTimeout(deadline);
                    cancelPending = undefined;
                    resolve(value);
                };
                const cancel = () => {
                    finish(false);
                    try {
                        call?.cancel();
                    }
                    catch {
                        /* cancellation cannot restart a stopped loop */
                    }
                };
                const deadline = setTimeout(() => {
                    if (!stopped)
                        logger.warn(`Heartbeat RPC timed out for run ${runId}`);
                    cancel();
                }, rpcTimeoutMs);
                cancelPending = cancel;
                try {
                    call = client.heartbeat({ runId, generation: String(generation) }, metadata, (err, resp) => {
                        if (settled || stopped)
                            return;
                        if (err) {
                            logger.warn(`Heartbeat RPC failed for run ${runId}:`, err.message);
                            finish(false);
                        }
                        else {
                            finish(resp.superseded);
                        }
                    });
                }
                catch (err) {
                    logger.warn(`Unexpected heartbeat error for run ${runId}:`, err);
                    finish(false);
                }
            });
            if (stopped)
                return;
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
            cancelPending?.();
        },
    };
}
