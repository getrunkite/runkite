/**
 * Shared TLS/mTLS configuration for connections FROM this runner TO the
 * control plane -- both the gRPC bridge (proto.ts's createRunnerClient)
 * and the proxy-mode HTTP calls (store.ts, vectorstore.ts, a2a.ts).
 * Direct TypeScript/undici port of the Python runner's tls_utils.py --
 * same env vars, same semantics, same defaults.
 *
 * - RUNKITE_TLS_CA_FILE: verify the control plane's server certificate
 *   against this CA instead of the system trust store (it REPLACES the
 *   system store for that verification, not "in addition to" -- the
 *   same behavior credentials.createSsl and undici's own Agent
 *   `connect.ca` both already have). Required for a self-signed or
 *   internal-CA-signed control plane cert.
 * - RUNKITE_GRPC_TLS: enables gRPC TLS using the system trust store
 *   when RUNKITE_TLS_CA_FILE is NOT set -- the gRPC equivalent of what
 *   an https:// URL already gives HTTP for free. gRPC has no URL
 *   scheme to carry that signal the way HTTP's http:// vs https:// does,
 *   so without this there would be no way to ask for "TLS, but a
 *   publicly-trusted cert, no custom CA needed" on the gRPC side at all
 *   -- only "plaintext" or "TLS with a specific custom CA". Passes
 *   `null` for rootCerts to credentials.createSsl, which falls back
 *   through gRPC's own bundled default roots to Node's own system trust
 *   store (see @grpc/grpc-js's own createSsl source). Ignored
 *   (redundant, not a conflict) when RUNKITE_TLS_CA_FILE is also set.
 *   HTTP proxy-mode calls need no equivalent: fetch already defaults to
 *   system-trust verification for any https:// base URL with nothing
 *   extra set.
 * - RUNKITE_TLS_CLIENT_CERT_FILE / RUNKITE_TLS_CLIENT_KEY_FILE: this
 *   runner's own client certificate for mTLS, when the control plane
 *   requires one (TLS_CLIENT_CA_FILE / GRPC_TLS_CLIENT_CA_FILE on the
 *   Go side -- see cmd/tls.go). Independent of which trust store is in
 *   use above -- mTLS and server-cert verification are orthogonal.
 *
 * All are optional and off by default -- unset envs mean exactly
 * today's plaintext gRPC / whatever the httpBaseUrl's own scheme already
 * implies for HTTP, matching every other env-var-driven piece of this
 * runner's config (RUNNER_TOKEN, RUNKITE_GRPC_URL, etc).
 */
import { readFileSync } from "node:fs";
import { isIP } from "node:net";
import { credentials } from "@grpc/grpc-js";
import { Agent, Dispatcher1Wrapper } from "undici";
function readIfSet(path) {
    return path ? readFileSync(path) : undefined;
}
function isTruthyEnv(value) {
    return /^(1|true|yes)$/i.test((value ?? "").trim());
}
export function requireWorkloadHTTPS(url) {
    let secure = false;
    try {
        secure = new URL(url).protocol === "https:";
    }
    catch {
        /* fixed safe error below */
    }
    if (!secure)
        throw new Error("configured client certificate requires HTTPS for control-plane requests");
}
/** Reject the current grpc-js IP SNI incompatibility before its polling loop. */
export function validateGrpcTLSTarget(address, nodeVersion = process.versions.node) {
    if (Number(nodeVersion.split(".")[0]) < 26)
        return;
    // These public resolver schemes contain literal IP addresses by definition.
    let literalIP = /^(?:ipv4|ipv6):/.test(address);
    const target = address.replace(/^dns:(?:\/\/[^/]*\/)?/, "");
    const host = target.startsWith("[")
        ? target.slice(1, target.indexOf("]"))
        : isIP(target)
            ? target
            : target.replace(/:[0-9]+$/, "");
    literalIP ||= isIP(host) !== 0;
    if (literalIP) {
        throw new Error("TypeScript gRPC TLS on Node.js 26 or newer requires a DNS endpoint with a matching certificate DNS SAN; current grpc-js sends unsupported IP SNI");
    }
}
class HTTPSOnlyAgent extends Agent {
    dispatch(options, handler) {
        requireWorkloadHTTPS(String(options.origin ?? ""));
        return super.dispatch(options, handler);
    }
}
/**
 * Returns TLS channel credentials for createRunnerClient, or undefined
 * if no CA, client certificate or RUNKITE_GRPC_TLS is configured. Callers
 * should fall back to credentials.createInsecure() in that case,
 * preserving today's default plaintext behavior exactly.
 *
 * rootCerts is undefined (system trust store, via createSsl's own
 * fallback chain) when RUNKITE_TLS_CA_FILE is unset but
 * RUNKITE_GRPC_TLS is -- see this module's own doc comment for why
 * that combination needs to exist at all.
 */
export function grpcChannelCredentials() {
    const caFile = process.env.RUNKITE_TLS_CA_FILE;
    const certFile = process.env.RUNKITE_TLS_CLIENT_CERT_FILE;
    const keyFile = process.env.RUNKITE_TLS_CLIENT_KEY_FILE;
    if (Boolean(certFile) !== Boolean(keyFile))
        throw new Error("gRPC mTLS requires both client certificate and key");
    if (!caFile && !certFile && !isTruthyEnv(process.env.RUNKITE_GRPC_TLS))
        return undefined;
    const rootCerts = readIfSet(caFile);
    const clientKey = readIfSet(process.env.RUNKITE_TLS_CLIENT_KEY_FILE);
    const clientCert = readIfSet(process.env.RUNKITE_TLS_CLIENT_CERT_FILE);
    return credentials.createSsl(rootCerts ?? null, clientKey ?? null, clientCert ?? null);
}
/**
 * Returns an undici dispatcher configured for TLS verification/mTLS against
 * the control plane's HTTP API, to pass as `{ dispatcher }` on fetch()
 * calls. Returns undefined when neither a custom CA nor client certificate is set;
 * otherwise client material also works with system server trust. Default
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
 * which importing undici's fetch export would silently stop being. Undici's
 * public Dispatcher1Wrapper adapts legacy built-in fetch callbacks (Node 22)
 * to the installed Undici 8 handler contract without changing global state.
 */
export function httpDispatcher() {
    const caFile = process.env.RUNKITE_TLS_CA_FILE;
    const certFile = process.env.RUNKITE_TLS_CLIENT_CERT_FILE;
    const keyFile = process.env.RUNKITE_TLS_CLIENT_KEY_FILE;
    if (keyFile && !certFile)
        throw new Error("HTTP mTLS requires a client certificate for its key");
    if (!caFile && !certFile)
        return undefined;
    const ClientAgent = certFile ? HTTPSOnlyAgent : Agent;
    return new Dispatcher1Wrapper(new ClientAgent({
        connect: {
            ca: readIfSet(caFile),
            cert: readIfSet(process.env.RUNKITE_TLS_CLIENT_CERT_FILE),
            key: readIfSet(process.env.RUNKITE_TLS_CLIENT_KEY_FILE),
        },
    }));
}
