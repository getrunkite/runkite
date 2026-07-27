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
}
export type RunStatus = "success" | "error" | "interrupted";
export declare function executeRun(adapter: LangGraphAdapter, assignment: RunAssignment, emit: (event: RunEvent) => Promise<void>, isCancelled: () => boolean): Promise<RunStatus>;
