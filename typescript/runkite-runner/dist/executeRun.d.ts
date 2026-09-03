/**
 * Executes a single run and emits RunEvents via a callback. TypeScript
 * mirror of the Python runner's execute_run() in worker.py -- same event
 * shapes, same lifecycle/interrupt/cancel/error handling, so the control
 * plane's normalizer sees an identical event stream regardless of which
 * runner produced it (this equivalence is the whole point of proving the
 * Runner Protocol is language-agnostic).
 */
import type { LangGraphAdapter } from "./adapter.js";
export interface RunEvent {
    event_id: string;
    seq: number;
    method: string;
    namespace: string[];
    data: unknown;
    ts: number;
}
export interface RunAssignment {
    run_id: string;
    thread_id: string;
    graph_id: string;
    input?: unknown;
    config?: Record<string, unknown>;
    stream_modes?: string[];
    checkpoint_ref?: string | null;
    resume_command?: {
        response?: unknown;
    } | null;
    user?: Record<string, unknown>;
    generation?: number;
    tenant_id?: string;
    allowed_tools?: string[] | null;
}
/**
 * Builds the RunnableConfig passed to graph.stream(), including the keys
 * LangGraph's own OSS code reads to populate Runtime.server_info for
 * node code -- distinct from a Factory Graph's ServerRuntime.user (see
 * factoryGraph.ts), which only the graph *factory* sees at build time.
 * LangGraph documents this as "the server puts assistant_id/graph_id in
 * config.configurable and the authenticated user dict in
 * configurable.langgraph_auth_user" -- any hosting server (LangGraph
 * Platform, this runner, or any other LangGraph SDK-compatible server)
 * is expected to set these keys for node-level code to work at all.
 * Direct TypeScript mirror of the Python runner's build_run_config() in
 * worker.py -- a pure function (no I/O) so it's unit-testable without a
 * live control plane or graph.
 */
export declare function buildRunConfig(assignment: RunAssignment): Record<string, any>;
export type RunStatus = "success" | "error" | "interrupted";
export declare function executeRun(adapter: LangGraphAdapter, assignment: RunAssignment, emit: (event: RunEvent) => Promise<void>, isCancelled: () => boolean): Promise<RunStatus>;
