/**
 * Shared TLS/mTLS configuration for connections FROM this runner TO the
 * control plane -- both the gRPC bridge (proto.ts's createRunnerClient)
 * and the proxy-mode HTTP calls (store.ts, vectorstore.ts, a2a.ts).
 * Direct TypeScript/undici port of the Python runner's tls_utils.py --
 * same three env vars, same semantics, same defaults.
 *
 * - RUNKITE_TLS_CA_FILE: verify the control plane's server certificate
 *   against this CA instead of (or in addition to) the system trust
 *   store. Required for a self-signed or internal-CA-signed control
 *   plane cert; a publicly-trusted cert needs nothing set at all --
 *   plain credentials.createSsl()/https:// URLs already verify against
 *   the system trust store on their own.
 * - RUNKITE_TLS_CLIENT_CERT_FILE / RUNKITE_TLS_CLIENT_KEY_FILE: this
 *   runner's own client certificate for mTLS, when the control plane
 *   requires one (TLS_CLIENT_CA_FILE / GRPC_TLS_CLIENT_CA_FILE on the
 *   Go side -- see cmd/tls.go).
 *
 * All three are optional and off by default -- unset envs mean exactly
 * today's plaintext gRPC / whatever the httpBaseUrl's own scheme already
 * implies for HTTP, matching every other env-var-driven piece of this
 * runner's config (RUNNER_TOKEN, RUNKITE_GRPC_URL, etc).
 */
import { readFileSync } from "node:fs";
import { credentials } from "@grpc/grpc-js";
import { Agent } from "undici";
function readIfSet(path) {
    return path ? readFileSync(path) : undefined;
}
/**
 * Returns TLS channel credentials for createRunnerClient, or undefined
 * if RUNKITE_TLS_CA_FILE is unset -- callers should fall back to
 * credentials.createInsecure() in that case, preserving today's default
 * plaintext behavior exactly.
 */
export function grpcChannelCredentials() {
    const caFile = process.env.RUNKITE_TLS_CA_FILE;
    if (!caFile)
        return undefined;
    const rootCerts = readFileSync(caFile);
    const clientKey = readIfSet(process.env.RUNKITE_TLS_CLIENT_KEY_FILE);
    const clientCert = readIfSet(process.env.RUNKITE_TLS_CLIENT_CERT_FILE);
    return credentials.createSsl(rootCerts, clientKey ?? null, clientCert ?? null);
}
/**
 * Returns an undici Agent configured for TLS verification/mTLS against
 * the control plane's HTTP API, to pass as `{ dispatcher }` on fetch()
 * calls -- or undefined if RUNKITE_TLS_CA_FILE is unset, in which case
 * callers should omit the dispatcher option entirely and let fetch use
 * its own default (system trust store), correct as-is for both a plain
 * http:// base URL (the CA is simply unused) and an https:// one with a
 * publicly-trusted certificate.
 *
 * A custom Agent is the only way to give Node's built-in fetch a custom
 * CA/client cert at all -- the RequestInit type has no ca/cert/key
 * fields of its own; see https://undici.nodejs.org/#/docs/api/Agent.
 * Deliberately still called through the GLOBAL fetch (not undici's own
 * exported fetch) at every call site -- Node's built-in fetch honors
 * this same `dispatcher` option (it's undici under the hood) while
 * staying interceptable by tests that mock globalThis.fetch directly,
 * which importing undici's fetch export would silently stop being.
 */
export function httpDispatcher() {
    const caFile = process.env.RUNKITE_TLS_CA_FILE;
    if (!caFile)
        return undefined;
    return new Agent({
        connect: {
            ca: readFileSync(caFile),
            cert: readIfSet(process.env.RUNKITE_TLS_CLIENT_CERT_FILE),
            key: readIfSet(process.env.RUNKITE_TLS_CLIENT_KEY_FILE),
        },
    });
}
