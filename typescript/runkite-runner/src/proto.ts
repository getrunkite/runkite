/**
 * Loads proto/runner.proto dynamically at runtime via @grpc/proto-loader --
 * no protoc/codegen step needed. Every message in the Runner Protocol is a
 * thin envelope around a JSON string (assignment_json, event_json), so the
 * loss of generated TypeScript types for message fields is minimal; this
 * keeps the package's build/install story as close to "just npm install
 * and run" as the Python runner's "just pip install and run" is.
 */
import { loadPackageDefinition, credentials, type Client } from "@grpc/grpc-js";
import { loadSync } from "@grpc/proto-loader";
import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { grpcChannelCredentials } from "./tls.js";

// proto/runner.proto is the same file Go/Python load. Prefer a copy
// shipped next to this package (npm install / packaged bin), then fall
// back to the monorepo layout (src/ and dist/ are siblings under
// typescript/runkite-runner/, both three levels below the repo root).
const __dirname = path.dirname(fileURLToPath(import.meta.url));
function resolveProtoPath(): string {
  const candidates = [
    path.resolve(__dirname, "../proto/runner.proto"),
    path.resolve(__dirname, "../../../proto/runner.proto"),
  ];
  for (const candidate of candidates) {
    if (existsSync(candidate)) return candidate;
  }
  throw new Error(
    `runner.proto not found (tried ${candidates.join(", ")}); ` +
      `keep the repo-root proto/ tree, or copy runner.proto to typescript/runkite-runner/proto/`,
  );
}
const PROTO_PATH = resolveProtoPath();

const packageDefinition = loadSync(PROTO_PATH, {
  keepCase: false, // camelCase field names: runnerKind, hasJob, etc.
  longs: String,
  enums: String,
  defaults: true,
  oneofs: true,
});

const proto = loadPackageDefinition(packageDefinition) as any;
const RunnerServiceCtor = proto.runkite.runner.v0.RunnerService;

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
  getJob(
    req: GetJobRequest,
    metadata: import("@grpc/grpc-js").Metadata,
    callback: (err: Error | null, resp: GetJobResponse) => void,
  ): void;
  streamEvents(
    metadata: import("@grpc/grpc-js").Metadata,
    callback: (err: Error | null, resp: StreamEventsResponse) => void,
  ): import("@grpc/grpc-js").ClientWritableStream<RunEventProto>;
  reportStatus(
    req: ReportStatusRequest,
    metadata: import("@grpc/grpc-js").Metadata,
    callback: (err: Error | null, resp: ReportStatusResponse) => void,
  ): void;
  watchCancels(
    req: WatchCancelsRequest,
    metadata: import("@grpc/grpc-js").Metadata,
  ): import("@grpc/grpc-js").ClientReadableStream<CancelSignal>;
  heartbeat(
    req: HeartbeatRequest,
    metadata: import("@grpc/grpc-js").Metadata,
    callback: (err: Error | null, resp: HeartbeatResponse) => void,
  ): void;
}

/**
 * Creates a RunnerService client against grpcAddress. TLS is opt-in via
 * RUNKITE_TLS_CA_FILE (see tls.ts's own doc comment) -- unset means
 * exactly the original plaintext createInsecure(), matching the Python
 * runner's identical worker.py/generic_worker.py fallback.
 */
export function createRunnerClient(grpcAddress: string): RunnerServiceClient {
  const creds = grpcChannelCredentials() ?? credentials.createInsecure();
  return new RunnerServiceCtor(grpcAddress, creds, {
    // Matches cmd/serve.go's keepalive.ServerParameters and the Python
    // runner's grpc.keepalive_time_ms -- detects a dead/crashed control
    // plane connection quickly instead of relying on TCP-level detection.
    "grpc.keepalive_time_ms": 2000,
    "grpc.keepalive_timeout_ms": 2000,
    "grpc.keepalive_permit_without_calls": 1,
  }) as RunnerServiceClient;
}
