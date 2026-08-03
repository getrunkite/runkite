/**
 * Pre-execution status check (Runner Protocol §10.3).
 * After GetJob dequeues a RunAssignment, ask the control plane whether
 * the run is still executable before starting agent work. Fail-open on
 * transport errors -- WatchCancels remains the primary cancel path.
 */
import { logger } from "./logger.js";
import { httpDispatcher } from "./tls.js";

const SKIP_STATUSES = new Set(["interrupted", "success", "error", "timeout"]);

type FetchInit = RequestInit & { dispatcher?: unknown };

export async function shouldSkipRun(
  httpAddress: string,
  runId: string,
  opts?: { runnerKind?: string; runnerToken?: string },
): Promise<boolean> {
  const base = httpAddress.replace(/\/+$/, "");
  const headers: Record<string, string> = {};
  if (opts?.runnerToken) {
    headers["X-Runner-Kind"] = opts.runnerKind || "typescript-langgraphjs";
    headers["X-Runner-Token"] = opts.runnerToken;
  }
  try {
    const init: FetchInit = { headers, dispatcher: httpDispatcher() };
    const resp = await fetch(`${base}/internal/runs/${runId}/status`, init);
    if (resp.status === 404) {
      logger.warn(`pre-exec status: run ${runId} not found; discarding`);
      return true;
    }
    if (!resp.ok) {
      logger.warn(`pre-exec status: run ${runId} HTTP ${resp.status}; proceeding (fail-open)`);
      return false;
    }
    const body = (await resp.json()) as { status?: string };
    const status = body.status ?? "";
    if (SKIP_STATUSES.has(status)) {
      logger.info(`pre-exec status: run ${runId} is ${status}; discarding assignment`);
      return true;
    }
    return false;
  } catch (err) {
    logger.warn(`pre-exec status: run ${runId} check failed (${err}); proceeding (fail-open)`);
    return false;
  }
}
