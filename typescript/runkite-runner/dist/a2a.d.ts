/**
 * Agent-to-Agent (A2A) delegation client: agent calls agent via the same
 * Agent Protocol API. TypeScript mirror of the Python runner's a2a.py --
 * see that file's docstring for the full rationale; repeated briefly here:
 *
 * `callAgent` is what a running agent's own node code calls to invoke
 * another agent as a sub-task -- it POSTs to the control plane's
 * `/internal/a2a/runs` endpoint (see internal/api/a2a.go for the full
 * server-side design: auth propagation, recursion limits, cost
 * attribution via root_run_id).
 *
 * Deliberately takes the graph node's own `config`, not separate run_id/
 * user parameters -- every value this needs (the current run_id to set
 * as parent_run_id, the authenticated user to forward as on_behalf_of)
 * is already there, set in executeRun.ts's buildRunConfig for every run.
 * A node function just calls
 * `await callAgent(config, "other_agent", { messages: [...] })`.
 *
 * Operational note (same as Python): with `wait: true` (the default),
 * the parent run's worker slot stays occupied until the child finishes.
 * A runner process with the default `--concurrency 1` therefore
 * deadlocks on nested A2A -- the child job cannot be dequeued until the
 * parent frees its slot. Use `--concurrency >= 2` (or another runner
 * replica of the same runner_kind, or `wait: false` + polling the
 * child run yourself) for any graph that delegates synchronously.
 */
export declare class A2AError extends Error {
}
export interface CallAgentOptions {
    wait?: boolean;
    threadId?: string;
    runConfig?: Record<string, unknown>;
    controlPlaneUrl?: string;
    /** AbortSignal-based timeout, the fetch equivalent of Python's httpx
     * `timeout` kwarg -- undefined (default) means no timeout, matching
     * Python: a wait=true call blocks for however long the sub-agent
     * actually takes, which the caller controls via its own input, not an
     * arbitrary client-side cutoff. */
    timeoutMs?: number;
}
/**
 * Invoke another agent as a sub-task from within a running agent's own
 * node code.
 *
 * @param config The RunnableConfig LangGraph.js passes to every node --
 *   must be the same `config` the calling node itself received (or its
 *   unmodified `configurable` sub-object), so run_id/user can be
 *   forwarded correctly.
 * @param agentId Which agent to delegate to.
 * @param input The sub-agent's input, same shape as a normal run's input.
 *
 * Returns, when `wait` is true (default): `{run: {...}, values: {...}}`
 * -- the sub-run's final state and output, same shape as the
 * client-facing `/runs/{id}/wait` response. When `wait` is false: just
 * the created run object, status "pending".
 *
 * Throws `A2AError` if the control plane rejects the call (e.g.
 * recursion depth exceeded, unknown parent_run_id), or a plain `Error`
 * if `config` has no `configurable.run_id` -- this wasn't called from
 * within an actual graph node's execution (or with a config that
 * executeRun's buildRunConfig never touched).
 */
export declare function callAgent(config: Record<string, any> | null | undefined, agentId: string, input: Record<string, unknown>, opts?: CallAgentOptions): Promise<Record<string, unknown>>;
