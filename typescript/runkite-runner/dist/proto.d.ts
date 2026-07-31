/**
 * Loads proto/runner.proto dynamically at runtime via @grpc/proto-loader --
 * no protoc/codegen step needed. Every message in the Runner Protocol is a
 * thin envelope around a JSON string (assignment_json, event_json), so the
 * loss of generated TypeScript types for message fields is minimal; this
 * keeps the package's build/install story as close to "just npm install
 * and run" as the Python runner's "just pip install and run" is.
 */
import { type Client } from "@grpc/grpc-js";
export interface GetJobRequest {
    runnerKind: string;
    timeoutSeconds: number;
}
export interface GetJobResponse {
    hasJob: boolean;
    assignmentJson: string;
}
export interface RunEventProto {
    runId: string;
    eventJson: string;
    /** See HeartbeatRequest.generation's own doc comment -- same fencing field, threaded through every RPC that identifies a specific dispatch attempt. */
    generation: string;
}
export interface StreamEventsResponse {
    ok: boolean;
}
export interface ReportStatusRequest {
    runId: string;
    status: string;
    errorMessage: string;
    generation: string;
}
export interface ReportStatusResponse {
    ok: boolean;
    /** See HeartbeatResponse.superseded's own doc comment. Informational here -- the runner already finished, nothing left to do besides logging it. */
    superseded: boolean;
}
export interface WatchCancelsRequest {
    runnerKind: string;
}
export interface CancelSignal {
    runId: string;
}
export interface HeartbeatRequest {
    runId: string;
    /**
     * The attempt number this runner was handed in its RunAssignment (see
     * proto/runner.proto's HeartbeatRequest.generation for the full
     * fencing rationale). A string here, not a number: @grpc/proto-loader's
     * `longs: String` option (this file's own loadSync config, chosen so a
     * genuinely large int64 doesn't silently lose precision going through
     * JS's float64 numbers) means every proto int64 field arrives as a
     * string on this client -- callers do a plain numeric comparison
     * (`Number(generation)`) since generations are small monotonic
     * counters, never large enough for that conversion to lose precision.
     */
    generation: string;
}
export interface HeartbeatResponse {
    ok: boolean;
    /**
     * True when this run's generation is no longer current -- a newer
     * attempt already exists (this runner was reclaimed while genuinely
     * still executing). The runner should treat this like a
     * self-triggered cancel: stop executing and report "interrupted"
     * instead of continuing to burn resources on a run already retried
     * elsewhere.
     */
    superseded: boolean;
}
/**
 * Minimal typed surface of the generated RunnerService client we actually
 * use. A @grpc/proto-loader-generated client's methods are shaped
 * dynamically from the .proto at load time, so this interface is written
 * by hand to match rpc definitions in proto/runner.proto -- not derived
 * from generated code, since there is none. Call signatures follow
 * @grpc/grpc-js's Client.makeUnaryRequest / makeClientStreamRequest /
 * makeServerStreamRequest conventions (verified against
 * node_modules/@grpc/grpc-js's own .d.ts).
 */
export interface RunnerServiceClient extends Client {
    getJob(req: GetJobRequest, metadata: import("@grpc/grpc-js").Metadata, callback: (err: Error | null, resp: GetJobResponse) => void): void;
    streamEvents(metadata: import("@grpc/grpc-js").Metadata, callback: (err: Error | null, resp: StreamEventsResponse) => void): import("@grpc/grpc-js").ClientWritableStream<RunEventProto>;
    reportStatus(req: ReportStatusRequest, metadata: import("@grpc/grpc-js").Metadata, callback: (err: Error | null, resp: ReportStatusResponse) => void): void;
    watchCancels(req: WatchCancelsRequest, metadata: import("@grpc/grpc-js").Metadata): import("@grpc/grpc-js").ClientReadableStream<CancelSignal>;
    heartbeat(req: HeartbeatRequest, metadata: import("@grpc/grpc-js").Metadata, callback: (err: Error | null, resp: HeartbeatResponse) => void): void;
}
/**
 * Creates a RunnerService client against grpcAddress. TLS is opt-in via
 * RUNKITE_TLS_CA_FILE (see tls.ts's own doc comment) -- unset means
 * exactly the original plaintext createInsecure(), matching the Python
 * runner's identical worker.py/generic_worker.py fallback.
 */
export declare function createRunnerClient(grpcAddress: string): RunnerServiceClient;
