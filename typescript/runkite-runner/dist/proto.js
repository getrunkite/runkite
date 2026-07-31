/**
 * Loads proto/runner.proto dynamically at runtime via @grpc/proto-loader --
 * no protoc/codegen step needed. Every message in the Runner Protocol is a
 * thin envelope around a JSON string (assignment_json, event_json), so the
 * loss of generated TypeScript types for message fields is minimal; this
 * keeps the package's build/install story as close to "just npm install
 * and run" as the Python runner's "just pip install and run" is.
 */
import { loadPackageDefinition, credentials } from "@grpc/grpc-js";
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
function resolveProtoPath() {
    const candidates = [
        path.resolve(__dirname, "../proto/runner.proto"),
        path.resolve(__dirname, "../../../proto/runner.proto"),
    ];
    for (const candidate of candidates) {
        if (existsSync(candidate))
            return candidate;
    }
    throw new Error(`runner.proto not found (tried ${candidates.join(", ")}); ` +
        `keep the repo-root proto/ tree, or copy runner.proto to typescript/runkite-runner/proto/`);
}
const PROTO_PATH = resolveProtoPath();
const packageDefinition = loadSync(PROTO_PATH, {
    keepCase: false, // camelCase field names: runnerKind, hasJob, etc.
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
});
const proto = loadPackageDefinition(packageDefinition);
const RunnerServiceCtor = proto.runkite.runner.v0.RunnerService;
/**
 * Creates a RunnerService client against grpcAddress. TLS is opt-in via
 * RUNKITE_TLS_CA_FILE (see tls.ts's own doc comment) -- unset means
 * exactly the original plaintext createInsecure(), matching the Python
 * runner's identical worker.py/generic_worker.py fallback.
 */
export function createRunnerClient(grpcAddress) {
    const creds = grpcChannelCredentials() ?? credentials.createInsecure();
    return new RunnerServiceCtor(grpcAddress, creds, {
        // Matches cmd/serve.go's keepalive.ServerParameters and the Python
        // runner's grpc.keepalive_time_ms -- detects a dead/crashed control
        // plane connection quickly instead of relying on TCP-level detection.
        "grpc.keepalive_time_ms": 2000,
        "grpc.keepalive_timeout_ms": 2000,
        "grpc.keepalive_permit_without_calls": 1,
    });
}
