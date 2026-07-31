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
