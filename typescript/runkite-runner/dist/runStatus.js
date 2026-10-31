/**
 * Pre-execution status check (Runner Protocol §10.3).
 * After GetJob dequeues a RunAssignment, ask the control plane whether
 * the run is still executable before starting agent work. Fail-open on
 * transport errors -- WatchCancels remains the primary cancel path.
 */
import { logger } from "./logger.js";
import { tenantHeaders } from "./tenantCtx.js";
import { httpDispatcher } from "./tls.js";
const SKIP_STATUSES = new Set(["interrupted", "success", "error", "timeout"]);
export async function shouldSkipRun(httpAddress, runId, opts) {
    const base = httpAddress.replace(/\/+$/, "");
    // The status route uses tenant-scoped storage even though it does not require
    // an active run binding. Preserve the assignment's current tenant here too.
    const headers = tenantHeaders();
    if (opts?.runnerToken) {
        headers["X-Runner-Kind"] = opts.runnerKind || "typescript-langgraphjs";
        headers["X-Runner-Token"] = opts.runnerToken;
    }
    try {
        // Match Python's httpx timeout=5.0 -- undici's default headers/body
        // timeouts are ~300s, which would defeat fail-open on a stalled CP.
        const init = {
            redirect: "error",
            headers,
            dispatcher: httpDispatcher(),
            signal: AbortSignal.timeout(5_000),
        };
        const resp = await fetch(`${base}/internal/runs/${encodeURIComponent(runId)}/status`, init);
        if (resp.status === 404) {
            logger.warn(`pre-exec status: run ${runId} not found; discarding`);
            return true;
        }
        if (!resp.ok) {
            logger.warn(`pre-exec status: run ${runId} HTTP ${resp.status}; proceeding (fail-open)`);
            return false;
        }
        const body = (await resp.json());
        const status = body.status ?? "";
        if (SKIP_STATUSES.has(status)) {
            logger.info(`pre-exec status: run ${runId} is ${status}; discarding assignment`);
            return true;
        }
        return false;
    }
    catch (err) {
        logger.warn(`pre-exec status: run ${runId} check failed (${err}); proceeding (fail-open)`);
        return false;
    }
}
