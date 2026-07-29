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
}
export interface StreamEventsResponse {
    ok: boolean;
}
export interface ReportStatusRequest {
    runId: string;
    status: string;
    errorMessage: string;
}
export interface ReportStatusResponse {
    ok: boolean;
}
export interface WatchCancelsRequest {
    runnerKind: string;
}
export interface CancelSignal {
    runId: string;
}
export interface HeartbeatRequest {
    runId: string;
}
export interface HeartbeatResponse {
    ok: boolean;
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
/** Creates a RunnerService client against grpcAddress (insecure, matching the Python/Go runners -- gRPC transport security is out of scope, same as the rest of the Runner Protocol). */
export declare function createRunnerClient(grpcAddress: string): RunnerServiceClient;
