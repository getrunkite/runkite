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
}

/** Creates a RunnerService client against grpcAddress (insecure, matching the Python/Go runners -- gRPC transport security is out of scope, same as the rest of the Runner Protocol). */
export function createRunnerClient(grpcAddress: string): RunnerServiceClient {
  return new RunnerServiceCtor(grpcAddress, credentials.createInsecure(), {
    // Matches cmd/serve.go's keepalive.ServerParameters and the Python
    // runner's grpc.keepalive_time_ms -- detects a dead/crashed control
    // plane connection quickly instead of relying on TCP-level detection.
    "grpc.keepalive_time_ms": 2000,
    "grpc.keepalive_timeout_ms": 2000,
    "grpc.keepalive_permit_without_calls": 1,
  }) as RunnerServiceClient;
}
